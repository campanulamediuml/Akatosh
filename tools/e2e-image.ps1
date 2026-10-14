# 图片链路端到端验证：上传 → 带图发消息 → 确认图片以 vision 格式到达上游
# 用法： .\tools\e2e-image.ps1
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:28888'
$mock = 'http://127.0.0.1:28899'
$failed = 0
function Pass($t) { Write-Host "  [PASS] $t" -ForegroundColor Green }
function Fail($t) { Write-Host "  [FAIL] $t" -ForegroundColor Red; $script:failed++ }

function Send-Json($method, $url, $obj) {
    $json = if ($null -eq $obj) { '{}' } else { $obj | ConvertTo-Json -Depth 8 -Compress }
    Invoke-RestMethod -Method $method $url -ContentType 'application/json; charset=utf-8' -Body ([Text.Encoding]::UTF8.GetBytes($json))
}

Write-Host "`n=== 1. 造一张大图并上传 ===" -ForegroundColor Cyan
Add-Type -AssemblyName System.Drawing
$imgPath = Join-Path $env:TEMP "e2e-noise.png"
$bmp = New-Object System.Drawing.Bitmap 1600, 1200
$rnd = New-Object System.Random 12345
for ($y = 0; $y -lt 1200; $y += 1) {
    for ($x = 0; $x -lt 1600; $x += 1) {
        $bmp.SetPixel($x, $y, [System.Drawing.Color]::FromArgb($rnd.Next(256), $rnd.Next(256), $rnd.Next(256)))
    }
}
$bmp.Save($imgPath, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
$origKB = [math]::Round((Get-Item $imgPath).Length / 1KB)
"  测试图: $origKB KB（1600×1200 噪声图）"

$up = curl.exe -s -X POST "$base/api/uploads" -F "file=@$imgPath"
$att = ($up | ConvertFrom-Json).attachments[0]
"  上传结果: id=$($att.id) $($att.width)x$($att.height) $([math]::Round($att.size/1KB))KB"
"    thumb_url=$($att.thumb_url)"
if ($att.id -gt 0 -and $att.width -eq 1600) { Pass "上传成功，尺寸识别正确" } else { Fail "上传返回不对: $up" }

Write-Host "`n=== 2. 缩略图必须远小于原图（这是性能的关键）===" -ForegroundColor Cyan
$thumbPath = Join-Path $env:TEMP "e2e-thumb.jpg"
$rawPath = Join-Path $env:TEMP "e2e-raw.png"
curl.exe -s -o $thumbPath "$base$($att.thumb_url)" | Out-Null
curl.exe -s -o $rawPath "$base$($att.raw_url)" | Out-Null
$thumbKB = [math]::Round((Get-Item $thumbPath).Length / 1KB)
$rawKB = [math]::Round((Get-Item $rawPath).Length / 1KB)
"  缩略图 $thumbKB KB  原图 $rawKB KB  倍数 $([math]::Round($rawKB / [math]::Max($thumbKB,1),1))x"
if ($thumbKB -lt 100 -and $thumbKB -lt $rawKB) { Pass "缩略图足够小（$thumbKB KB）" } else { Fail "缩略图太大：$thumbKB KB" }
$thumbImg = [System.Drawing.Image]::FromFile($thumbPath)
"  缩略图尺寸: $($thumbImg.Width)x$($thumbImg.Height)"
if ($thumbImg.Width -eq 320) { Pass "缩略图长边压到 320" } else { Fail "缩略图尺寸不对: $($thumbImg.Width)x$($thumbImg.Height)" }
$thumbImg.Dispose()

Write-Host "`n=== 3. 缓存头 ===" -ForegroundColor Cyan
$h = curl.exe -s -I "$base$($att.thumb_url)"
$etag = ($h | Select-String -Pattern 'ETag: (.+)').Matches.Groups[1].Value.Trim()
$cc = ($h | Select-String -Pattern 'Cache-Control: (.+)').Matches.Groups[1].Value.Trim()
"  ETag=$etag"
"  Cache-Control=$cc"
if ($cc -match 'immutable') { Pass "内容寻址 → 可以 immutable 长缓存" } else { Fail "缺 Cache-Control" }
$code304 = curl.exe -s -o NUL -w "%{http_code}" -H "If-None-Match: $etag" "$base$($att.thumb_url)"
if ($code304 -eq '304') { Pass "带 If-None-Match 返回 304" } else { Fail "304 没生效，实际 $code304" }

Write-Host "`n=== 4. 同一张图重复上传应当去重 ===" -ForegroundColor Cyan
$up2 = curl.exe -s -X POST "$base/api/uploads" -F "file=@$imgPath"
$att2 = ($up2 | ConvertFrom-Json).attachments[0]
if ($att2.id -eq $att.id) { Pass "内容相同 → 复用同一条记录（id=$($att2.id)）" } else { Fail "没有去重: $($att.id) vs $($att2.id)" }

Write-Host "`n=== 5. 带图发消息，确认图片以 vision 格式到达上游 ===" -ForegroundColor Cyan
$conv = Send-Json POST "$base/api/conversations" @{}
$before = (Invoke-RestMethod "$mock/__stats").chat_requests
Send-Json POST "$base/api/conversations/$($conv.id)/messages" @{ content = '这张图里是什么？'; attachment_ids = @($att.id) } | Out-Null
$t0 = Get-Date
while (((Get-Date) - $t0).TotalSeconds -lt 60) {
    Start-Sleep -Milliseconds 400
    if ($null -eq (Invoke-RestMethod "$base/api/conversations/$($conv.id)/state").generating) { break }
}
$st = (Invoke-RestMethod "$mock/__stats")
"  上游统计: 请求数=$($st.chat_requests)（之前 $before）  收到图片=$($st.image_parts) 张 / $($st.last_image_kb) KB"
"  上游看到的用户文本: $($st.last_user_text)"
if ($st.image_parts -ge 1 -and $st.chat_requests -gt $before) { Pass "图片确实以 vision 多段 content 发到了上游" } else { Fail "上游没收到图片" }

Write-Host "`n=== 6. 恢复会话时 state 里只有 URL，没有图片数据 ===" -ForegroundColor Cyan
$state = Invoke-RestMethod "$base/api/conversations/$($conv.id)/state"
$msg = $state.messages | Where-Object { $_.role -eq 'user' } | Select-Object -First 1
$raw = (Invoke-WebRequest "$base/api/conversations/$($conv.id)/state" -UseBasicParsing).Content
"  用户消息带了 $($msg.attachments.Count) 个附件"
"  state 响应体大小: $([math]::Round($raw.Length/1KB,1)) KB（原图 $rawKB KB）"
if ($msg.attachments.Count -eq 1) { Pass "附件元数据随消息返回" } else { Fail "附件没带回来" }
if ($raw.Length -lt ($rawKB * 1024 / 4)) { Pass "state 里没有塞图片数据（响应体远小于原图）" } else { Fail "state 里可能夹带了图片数据" }

Write-Host "`n=== 7. 非图片文件要被拒 ===" -ForegroundColor Cyan
$txtPath = Join-Path $env:TEMP "e2e-notimage.txt"
Set-Content -Path $txtPath -Value "这不是图片" -Encoding UTF8
$bad = curl.exe -s -X POST "$base/api/uploads" -F "file=@$txtPath"
"  返回: $bad"
if ($bad -match '不是能识别的图片') { Pass "非图片被明确拒绝" } else { Fail "非图片没被拒绝: $bad" }

Remove-Item -Force $imgPath, $thumbPath, $rawPath, $txtPath -ErrorAction SilentlyContinue
Write-Host ""
if ($failed -eq 0) { Write-Host "===== 图片链路全部通过 =====" -ForegroundColor Green }
else { Write-Host "===== 有 $failed 项失败 =====" -ForegroundColor Red }
exit $failed
