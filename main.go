package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"aiassistant/internal/httpapi"
	"aiassistant/internal/imagegen"
	"aiassistant/internal/llm"
	"aiassistant/internal/media"
	"aiassistant/internal/sandbox"
	"aiassistant/internal/search"
	"aiassistant/internal/session"
	"aiassistant/internal/store"
)

// 前端构建产物直接嵌进二进制，部署就是一个 exe。
//
//go:embed all:web/dist
var webDist embed.FS

const version = "1.0.0"

func main() {
	addr := flag.String("addr", "127.0.0.1:28888", "监听地址（默认只绑本机；要让手机访问用 -addr 0.0.0.0:28888）")
	dataDir := flag.String("data", "data", "数据目录，存放 sqlite 数据库")
	miscDir := flag.String("misc", "misc", "附件目录，存放上传的图片（Docker 部署时可以单独挂载）")
	tokenFlag := flag.String("token", "", "访问令牌：留空不鉴权，填 auto 自动生成并打印。手机访问时强烈建议设置")
	allowHostFlag := flag.String("allow-host", "", "额外允许的主机名，逗号分隔（用机器名/MagicDNS 名访问时才需要；用 IP 不需要）")
	searxngFlag := flag.String("searxng", "", "SearXNG 地址（如 http://searxng:8080）：配置后开启联网搜索（function calling）；留空禁用")
	pyboxFlag := flag.String("pybox", "", "Python 沙箱地址（如 http://pybox:8000）：配置后开启 run_python 代码执行工具；留空禁用")
	imagegenFlag := flag.String("imagegen", "", "图像生成：格式 base_url|api_key|model（model 可省，默认 Tongyi-MAI/Z-Image-Turbo）；如 https://api.siliconflow.cn|sk-xxx|Kwai-Kolors/Kolors")
	compactFlag := flag.Float64("compact-threshold", 0.7, "上下文提示阈值（0~1）：进度条超过这个比例时变红并建议手动压缩（压缩本身是手动触发，见会话窗口右上角按钮；0 = 不提示）")
	openBrowser := flag.Bool("open", false, "启动后自动打开浏览器")
	flag.Parse()

	token := strings.TrimSpace(*tokenFlag)
	autoToken := strings.EqualFold(token, "auto")
	if autoToken {
		token = httpapi.GenerateToken()
		if token == "" {
			log.Printf("警告：生成访问令牌失败，首个管理员将使用自动生成的令牌")
		}
	}

	dbPath := filepath.Join(*dataDir, "ai-assistant.db")
	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()

	// 多租户引导：users 表为空时用 -token（或自动生成）创建首个管理员，
	// 并把单租户时代的全部数据划到其名下；之后鉴权完全走 users 表。
	adminToken, adminCreated, err := st.BootstrapUsers(token)
	if err != nil {
		log.Fatalf("初始化用户体系失败: %v", err)
	}

	// 图片存储：misc 目录可以单独挂到外部卷
	mediaStore, err := media.NewStore(*miscDir)
	if err != nil {
		log.Fatalf("初始化附件目录失败: %v", err)
	}

	// 上一次进程被杀掉时留下的 streaming 消息不可能再继续了，标成已中止
	if n, err := st.MarkInterrupted(); err != nil {
		log.Printf("清理中断的生成时出错: %v", err)
	} else if n > 0 {
		log.Printf("已将 %d 条被中断的生成标记为「已中止」", n)
	}

	bus := session.NewBus()
	// 客户端是按「供应商 id」现构造的：用户随时可能改某家的 URL / Key，
	// 而且一个部署可以配多家。
	mgr := session.NewManagerWithMedia(st, bus, func(userID, providerID int64) (*llm.Client, error) {
		p, err := st.ResolveProvider(userID, providerID)
		if err != nil {
			return nil, err
		}
		if p == nil || strings.TrimSpace(p.BaseURL) == "" {
			return nil, session.NewError("no_provider", "这家供应商还没填 API 地址")
		}
		return llm.New(p.BaseURL, p.APIKey), nil
	}, mediaStore)

	// 联网搜索（function calling）：配了 -searxng 才启用
	if sx := strings.TrimSpace(*searxngFlag); sx != "" {
		mgr.WithSearch(search.New(sx))
		log.Printf("  联网搜索 : %s（会话级开关，function calling）", sx)
	}
	// Python 代码执行：配了 -pybox 就全局可用（沙箱无副作用，模型自主判断用不用）
	if pb := strings.TrimSpace(*pyboxFlag); pb != "" {
		mgr.WithSandbox(sandbox.New(pb))
		log.Printf("  Python 沙箱 : %s（function calling，模型自主调用）", pb)
	}
	// 图像生成：-imagegen base_url|key|model
	if ig := strings.TrimSpace(*imagegenFlag); ig != "" {
		parts := strings.SplitN(ig, "|", 3)
		if len(parts) >= 2 && parts[0] != "" {
			model := "Tongyi-MAI/Z-Image-Turbo"
			if len(parts) == 3 && strings.TrimSpace(parts[2]) != "" {
				model = strings.TrimSpace(parts[2])
			}
			mgr.WithImageGen(imagegen.New(parts[0], parts[1], model))
			log.Printf("  图像生成 : %s（model=%s，function calling）", parts[0], model)
		} else {
			log.Printf("警告：-imagegen 格式应为 base_url|api_key|model，当前无法解析，图像生成未启用")
		}
	}
	// 上下文压缩：默认 0.7，0 = 禁用
	mgr.WithCompactThreshold(*compactFlag)
	if *compactFlag > 0 && *compactFlag < 1 {
		log.Printf("  上下文压缩 : 阈值 %.0f%%（达到后旧历史压为摘要并冻结）", *compactFlag*100)
	}

	sub, err := fs.Sub(webDist, "web/dist")
	if err != nil {
		log.Fatalf("嵌入的前端资源不可用: %v", err)
	}

	handler := httpapi.New(st, mgr, sub, version, *addr, mediaStore,
		httpapi.WithFilesDir(filepath.Join(*miscDir, "files")),
		httpapi.WithAllowHosts(splitList(*allowHostFlag)),
	)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout 必须是 0：SSE 是长连接，任何写超时都会把正在生成的回答砍断。
		WriteTimeout: 0,
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", *addr, err)
	}

	abs, _ := filepath.Abs(dbPath)
	miscAbs, _ := filepath.Abs(*miscDir)
	log.Printf("AI 助手 %s 已启动", version)
	log.Printf("  数据文件 : %s", abs)
	log.Printf("  附件目录 : %s", miscAbs)

	urls := accessTargets(*addr)
	first := true
	for _, t := range urls {
		line := t.url
		if t.note != "" {
			line += "  (" + t.note + ")"
		}
		if first {
			log.Printf("  访问地址 : %s", line)
			first = false
		} else {
			log.Printf("             %s", line)
		}
	}

	if handler.Exposed() {
		log.Printf("  已绑到非回环地址：同网络的设备都能访问")
		if adminCreated {
			log.Printf("  管理员访问令牌 : %s", adminToken)
			log.Printf("  手机上直接开这个地址即可（令牌只需用一次，之后靠 cookie）：")
			for _, t := range urls {
				if isLoopbackURL(t.url) {
					continue
				}
				log.Printf("             %s/?token=%s", t.url, adminToken)
			}
			log.Printf("  ⚠ 这行日志里含明文令牌，别往外贴")
			log.Printf("  更多用户：管理员登录后在「设置 → 用户管理」里创建（每人一个令牌）")
		}
	}
	log.Printf("  按 Ctrl+C 退出")

	if *openBrowser {
		go func() {
			time.Sleep(300 * time.Millisecond)
			target := "http://" + *addr
			for _, t := range urls {
				if !isLoopbackURL(t.url) {
					target = t.url
					break
				}
			}
			launchBrowser(target)
		}()
	}

	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	log.Printf("正在关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 先让各会话把正在生成的最后一小段内容落盘，再停 HTTP 服务
	mgr.Shutdown(ctx)
	_ = httpServer.Shutdown(ctx)
}

// splitList 把逗号分隔的参数拆成切片（去空、去空白）。
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// accessTarget 一个可直接访问的地址，note 只用来提示（比如网卡名）。
// 两者必须分开：早先把网卡名拼进 URL 里，结果打印成 "...:28888  (tun0)/?token=..."。
type accessTarget struct {
	url  string
	note string
}

// accessTargets 算出「真正能点开」的地址。
//
// 之前这里直接把 -addr 原样打出来，绑 0.0.0.0 时日志显示 http://0.0.0.0:28888 ——
// 而 0.0.0.0 不是一个能访问的目标地址，用户拿着这行字根本不知道该在手机上输什么。
func accessTargets(bindAddr string) []accessTarget {
	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		return []accessTarget{{url: "http://" + bindAddr}}
	}
	h := strings.Trim(host, "[]")
	ip := net.ParseIP(h)

	switch {
	case ip != nil && ip.IsUnspecified(), h == "":
		// 绑了所有地址：本机回环 + 每一张网卡的 IPv4 都能用，手机要用后者
		out := []accessTarget{{url: "http://" + net.JoinHostPort("127.0.0.1", port), note: "本机"}}
		for _, ni := range localIPv4s() {
			out = append(out, accessTarget{url: "http://" + net.JoinHostPort(ni.ip, port), note: ni.name})
		}
		return out

	case h != "" && strings.EqualFold(h, "localhost"), ip != nil && ip.IsLoopback():
		// 只绑回环：局域网访问不了，如实打出来，别给用户错觉
		return []accessTarget{{url: "http://" + net.JoinHostPort("127.0.0.1", port), note: "仅本机，手机访问不了"}}

	default:
		return []accessTarget{{url: "http://" + net.JoinHostPort(h, port)}}
	}
}

type nicIPv4 struct {
	name string
	ip   string
}

// localIPv4s 列出本机所有已启用、非回环的 IPv4 地址，附带网卡名。
// 带网卡名是因为一台机器常有多个地址（Hyper-V 默认交换机、VPN 虚拟网卡…），
// 用户得靠名字判断手机上该用哪个。
func localIPv4s() []nicIPv4 {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []nicIPv4
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, nicIPv4{name: ifc.Name, ip: ip4.String()})
		}
	}
	return out
}

// isLoopbackURL 判断地址是不是本机回环（用来跳过它，只给手机列可用的）。
func isLoopbackURL(u string) bool {
	s := strings.TrimPrefix(u, "http://")
	s = strings.TrimPrefix(s, "https://")
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i] // 去掉 "  (网卡名)" 后缀
	}
	h, _, err := net.SplitHostPort(s)
	if err != nil {
		h = s
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func launchBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("打开浏览器失败（请手动访问 %s）: %v", url, err)
	}
}
