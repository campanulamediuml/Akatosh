# 端到端验证脚本：需要先启动 mock 上游(28899)和本服务(28888)
#   go run ./tools/mockllm -addr 127.0.0.1:28899 -delay 120ms
#   .\ai-assistant.exe -addr 127.0.0.1:28888 -data .\data-test
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:28888'

function Send-Json($method, $url, $obj) {
    $json = if ($null -eq $obj) { '{}' } else { $obj | ConvertTo-Json -Depth 8 -Compress }
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    Invoke-RestMethod -Method $method $url -ContentType 'application/json; charset=utf-8' -Body $bytes
}
function Show-Err($e) {
    $msg = $e.ErrorDetails.Message
    if (-not $msg) { $msg = $e.Exception.Message }
    "      HTTP $([int]$e.Exception.Response.StatusCode)  $msg"
}
function Pass($t) { Write-Host "  [PASS] $t" -ForegroundColor Green }
function Fail($t) { Write-Host "  [FAIL] $t" -ForegroundColor Red; $script:failed++ }
$failed = 0

Write-Host "`n=== 0. 配置供应商（多供应商：这里先建/复用第一个）===" -ForegroundColor Cyan
$existing = Invoke-RestMethod "$base/api/providers"
if ($existing.Count -gt 0) {
    $script:providerId = $existing[0].id
    "  复用已有供应商 id=$($script:providerId) name=$($existing[0].name)"
} else {
    $p = Send-Json POST "$base/api/providers" @{ name = '本地mock'; base_url = 'http://127.0.0.1:28899/v1'; api_key = 'test-key-123456' }
    $script:providerId = $p.id
    "  已创建供应商 id=$($script:providerId)"
}
$mf = Send-Json POST "$base/api/providers/$($script:providerId)/models/fetch" @{}
"  拉到模型 $($mf.total) 个（新增 $($mf.added)）"
if ($mf.total -gt 0) { Pass "供应商可用，模型 $($mf.total) 个" } else { Fail "没拉到模型" }

Write-Host "`n=== 1. 创建会话 ===" -ForegroundColor Cyan
$conv = Send-Json POST "$base/api/conversations" @{ title = '端到端测试'; model_id = 'mock-reasoner'; effort = 'high' }
"  会话 id=$($conv.id) 标题=$($conv.title) 供应商=$($conv.provider_name) 模型=$($conv.model_id) 强度=$($conv.effort)"
if ($conv.title -eq '端到端测试') { Pass "中文标题往返正确" } else { Fail "中文标题被破坏: $($conv.title)" }
if ($conv.provider_id -eq $script:providerId) { Pass "会话绑定了正确的供应商" } else { Fail "provider_id 不对: $($conv.provider_id)" }

Write-Host "`n=== 2. 建立唯一的 SSE 长连接 ===" -ForegroundColor Cyan
$sseFile = Join-Path $PWD 'sse-capture.txt'
Remove-Item $sseFile -ErrorAction SilentlyContinue
$curl = Start-Process -FilePath 'curl.exe' -ArgumentList @('-N', '-s', '--max-time', '40', "$base/api/stream") `
    -RedirectStandardOutput $sseFile -PassThru -NoNewWindow
Start-Sleep -Milliseconds 900
$clients = (Invoke-RestMethod "$base/api/health").stream_clients
"  已连接客户端数 = $clients"
if ($clients -ge 1) { Pass "SSE 连接已建立（全局仅此一条）" } else { Fail "SSE 连接未建立" }

Write-Host "`n=== 3. 发送消息 ===" -ForegroundColor Cyan
$chinese = '你好，请用中文回答：1+1 等于几？顺便写个 Go 的 Hello World。'
$send1 = Send-Json POST "$base/api/conversations/$($conv.id)/messages" @{ content = $chinese; model_id = 'mock-reasoner'; effort = 'high' }
"  已接受 user_message_id=$($send1.user_message_id) assistant_message_id=$($send1.assistant_message_id)"

Write-Host "`n=== 4. 生成途中再发一条（应被丢弃）===" -ForegroundColor Cyan
try {
    $send2 = Send-Json POST "$base/api/conversations/$($conv.id)/messages" @{ content = '这条多出来的消息应该被丢弃'; model_id = 'mock-reasoner' }
    Fail "第二条消息竟然被接受了: $($send2 | ConvertTo-Json -Compress)"
} catch {
    Show-Err $_
    if ($_.ErrorDetails.Message -match '"code":"busy"') { Pass "返回 busy，消息被丢弃" } else { Fail "错误码不是 busy" }
}

Write-Host "`n=== 5. 生成途中点停止 ===" -ForegroundColor Cyan
Start-Sleep -Milliseconds 600
$stop = Send-Json POST "$base/api/conversations/$($conv.id)/stop" @{}
"  stopped=$($stop.stopped)"
if ($stop.stopped) { Pass "停止命令生效" } else { Fail "停止命令无效" }
Start-Sleep -Milliseconds 300

$st = Invoke-RestMethod "$base/api/conversations/$($conv.id)/state"
$asst = $st.messages | Where-Object { $_.role -eq 'assistant' } | Select-Object -First 1
"  助手消息 status=$($asst.status) 正文长度=$($asst.content.Length) CoT长度=$($asst.reasoning.Length)"
if ($asst.status -eq 'aborted') { Pass "中止状态已落库" } else { Fail "状态应为 aborted，实际 $($asst.status)" }
if ($asst.reasoning.Length -gt 0) { Pass "中止时已生成的 CoT 被保留" } else { Fail "中止后 CoT 丢失" }
if ($st.generating -eq $null) { Pass "generating 已清空" } else { Fail "generating 未清空" }

Write-Host "`n=== 6. 正常生成一轮（验证 CoT + 正文 + 用量）===" -ForegroundColor Cyan
$send3 = Send-Json POST "$base/api/conversations/$($conv.id)/messages" @{ content = '再来一次，这次不要停'; model_id = 'mock-reasoner'; effort = 'high' }
$t0 = Get-Date
while ($true) {
    Start-Sleep -Milliseconds 300
    $st = Invoke-RestMethod "$base/api/conversations/$($conv.id)/state"
    if ($null -eq $st.generating) { break }
    if (((Get-Date) - $t0).TotalSeconds -gt 25) { Fail "生成超时"; break }
}
$asst = $st.messages | Where-Object { $_.role -eq 'assistant' } | Select-Object -Last 1
"  status=$($asst.status) 正文=$($asst.content.Length) 字  CoT=$($asst.reasoning.Length) 字"
"  usage: prompt=$($asst.usage.prompt_tokens) completion=$($asst.usage.completion_tokens) reasoning=$($asst.usage.reasoning_tokens)"
if ($asst.status -eq 'done') { Pass "正常完成" } else { Fail "状态应为 done，实际 $($asst.status)" }
if ($asst.reasoning -like '*多字节字符不能被切断*') { Pass "CoT 中文完整（增量切片没有切坏多字节字符）" } else { Fail "CoT 中文异常" }
if ($asst.content -like '*你好，世界*') { Pass "正文中文完整" } else { Fail "正文中文异常: $($asst.content)" }
if ($asst.usage.reasoning_tokens -eq 96) { Pass "用量已记录" } else { Fail "用量缺失" }

Write-Host "`n=== 7. 编辑正文（后端为准）===" -ForegroundColor Cyan
$edited = Send-Json PATCH "$base/api/messages/$($asst.id)" @{ content = '这是我自己改写的正文。' }
"  edited=$($edited.message.edited) content=$($edited.message.content)"
if ($edited.message.edited -and $edited.message.content -eq '这是我自己改写的正文。') { Pass "编辑成功且标记 edited" } else { Fail "编辑异常" }
if ($edited.message.content_original.Length -gt 0) { Pass "原文已保留（可还原）" } else { Fail "原文未保留" }

Write-Host "`n=== 8. 编辑内容进入下一轮历史 ===" -ForegroundColor Cyan
$before = (Get-Date)
$send4 = Send-Json POST "$base/api/conversations/$($conv.id)/messages" @{ content = '收到'; model_id = 'mock-reasoner' }
$t0 = Get-Date
while ($true) {
    Start-Sleep -Milliseconds 300
    $st = Invoke-RestMethod "$base/api/conversations/$($conv.id)/state"
    if ($null -eq $st.generating) { break }
    if (((Get-Date) - $t0).TotalSeconds -gt 25) { Fail "生成超时"; break }
}
Pass "第二轮完成（历史组装未因编辑而报错）"

Write-Host "`n=== 9. 还原原文 ===" -ForegroundColor Cyan
$restored = Send-Json POST "$base/api/messages/$($asst.id)/restore" @{}
if (-not $restored.message.edited -and $restored.message.content -like '*你好，世界*') { Pass "还原成功" } else { Fail "还原失败: $($restored.message.content)" }

Write-Host "`n=== 10. 重新生成（截断）===" -ForegroundColor Cyan
$reg = Send-Json POST "$base/api/messages/$($asst.id)/regenerate" @{}
"  accepted=$($reg.accepted)"
$t0 = Get-Date
while ($true) {
    Start-Sleep -Milliseconds 300
    $st = Invoke-RestMethod "$base/api/conversations/$($conv.id)/state"
    if ($null -eq $st.generating -and $st.messages.Count -gt 0) { break }
    if (((Get-Date) - $t0).TotalSeconds -gt 25) { Fail "重新生成超时"; break }
}
"  截断后消息数=$($st.messages.Count)"
$st.messages | ForEach-Object { "    [{0}] {1,-9} status={2,-9} content={3}字" -f $_.id, $_.role, $_.status, $_.content.Length }
if ($st.messages.Count -eq 4 -and $st.messages[-1].role -eq 'assistant' -and $st.messages[-1].id -gt 6) { Pass "截断正确：目标及其后消息被删除并重新生成" } else { Fail "截断后应剩 4 条（1/2/3 + 新生成的），实际 $($st.messages.Count)" }

Write-Host "`n=== 11. 被丢弃的消息确实没有落库 ===" -ForegroundColor Cyan
$all = (Invoke-RestMethod "$base/api/conversations/$($conv.id)/state").messages
$bad = $all | Where-Object { $_.content -like '*应该被丢弃*' }
if ($null -eq $bad) { Pass "被丢弃的消息不在库里" } else { Fail "被丢弃的消息竟然落库了" }

Write-Host "`n=== 12. 鉴权失败的错误信息 ===" -ForegroundColor Cyan
Send-Json PATCH "$base/api/providers/$($script:providerId)" @{ api_key = 'bad-key' } | Out-Null
try {
    Send-Json POST "$base/api/providers/$($script:providerId)/models/fetch" @{} | Out-Null
    Fail "坏 key 竟然成功了"
} catch {
    Show-Err $_
    if ($_.ErrorDetails.Message -match 'Incorrect API key') { Pass "上游错误信息被完整透传" } else { Fail "错误信息丢失" }
}
Send-Json PATCH "$base/api/providers/$($script:providerId)" @{ api_key = 'test-key-123456' } | Out-Null

Write-Host "`n=== 13. 不支持的模型不发 reasoning_effort（自动降级重试）===" -ForegroundColor Cyan
$conv2 = Send-Json POST "$base/api/conversations" @{ title = 'plain 测试'; model_id = 'plain-model'; effort = '' }
$sendP = Send-Json POST "$base/api/conversations/$($conv2.id)/messages" @{ content = '测试明文模型'; model_id = 'plain-model'; effort = 'high' }
$t0 = Get-Date
while ($true) {
    Start-Sleep -Milliseconds 300
    $stP = Invoke-RestMethod "$base/api/conversations/$($conv2.id)/state"
    if ($null -eq $stP.generating) { break }
    if (((Get-Date) - $t0).TotalSeconds -gt 25) { break }
}
$asstP = $stP.messages | Where-Object { $_.role -eq 'assistant' } | Select-Object -First 1
"  status=$($asstP.status) error=$($asstP.error)"
if ($asstP.status -eq 'done') { Pass "该模型被识破不支持思考，effort 未下发" } else { Fail "应成功，实际 $($asstP.status) $($asstP.error)" }

Start-Sleep -Milliseconds 500
if (-not $curl.HasExited) { Stop-Process -Id $curl.Id -Force -ErrorAction SilentlyContinue }
Start-Sleep -Milliseconds 300

Write-Host "`n=== 14. SSE 流内容 ===" -ForegroundColor Cyan
$sse = Get-Content $sseFile -Raw -Encoding UTF8
$events = [regex]::Matches($sse, '(?m)^event: (\w+)')
$byType = $events | Group-Object { $_.Groups[1].Value } | ForEach-Object { "$($_.Name)=$($_.Count)" }
"  事件统计: $($byType -join '  ')"
"  心跳数: $(([regex]::Matches($sse, '(?m)^: ping')).Count)"
foreach ($t in @('hello', 'delta', 'message', 'conv')) {
    if ($events | Where-Object { $_.Groups[1].Value -eq $t }) { Pass "收到 $t 事件" } else { Fail "没有收到 $t 事件" }
}
if ($sse -match '"generating":true') { Pass "生成开始时广播了 generating=true 的 conv 事件（左侧列表标记可信）" } else { Fail "缺少 generating=true 的 conv 事件" }
if ($sse -match '"generating":false') { Pass "生成结束时广播了 generating=false" } else { Fail "缺少 generating=false 的 conv 事件" }
$seqs = [regex]::Matches($sse, '"seq":(\d+)') | ForEach-Object { [int64]$_.Groups[1].Value }
$mono = $true
for ($i = 1; $i -lt $seqs.Count; $i++) { if ($seqs[$i] -le $seqs[$i - 1]) { $mono = $false } }
if ($mono -and $seqs.Count -gt 5) { Pass "seq 全局单调递增（$($seqs.Count) 个）" } else { Fail "seq 不是单调递增" }

Write-Host "`n=== 15. 会话列表的「正在生成」标记 ===" -ForegroundColor Cyan
$list = Invoke-RestMethod "$base/api/conversations"
$list | ForEach-Object { "    [{0}] {1,-14} msgs={2} generating={3}" -f $_.id, $_.title, $_.message_count, $_.generating }
Pass "会话列表可读"

Write-Host ""
if ($failed -eq 0) {
    Write-Host "===== 全部通过 =====" -ForegroundColor Green
} else {
    Write-Host "===== 有 $failed 项失败 =====" -ForegroundColor Red
}
exit $failed
