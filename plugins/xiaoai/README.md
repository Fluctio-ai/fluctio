# xiaoai plugin

把小爱音箱变成 FastClaw 的对话渠道（channel 型插件）。通过 NAS 上的
[Songloft](https://github.com/songloft-org/songloft)（miot 智能音箱插件）桥接：

```
你说话 → 小爱云 ASR → Songloft 对话监听（轮询+去重，miot 插件负责）
  → POST webhook 到本插件 → message.inbound → FastClaw agent turn
agent 回复 → channel.send → 本插件 → POST /mina/tts → 音箱播报
```

- 零 npm 依赖（Node ≥ 18，FastClaw 容器镜像自带 node）
- 小爱账号凭证由 Songloft 管理，本插件不碰小米云
- 轮询/去重/设备管理全部由 Songloft miot 插件完成，本插件只做转发

## 前置

1. Songloft 已部署并登录（NAS）
2. miot 智能音箱插件已安装、小米账号已登录、设备已纳管
   （插件页 → 对话监听已工作）
3. FastClaw 与 Songloft 网络互通（同一 NAS / docker 网络用容器名）

## 安装

把本目录放进 FastClaw 的插件目录（默认 `~/.fluctio/plugins/xiaoai/`），
在 FastClaw 配置中启用 plugins 并填入本插件 config：

```json
{
  "songloftUrl": "http://songloft:5045",
  "songloftUsername": "你的 Songloft 用户名",
  "songloftPassword": "你的 Songloft 密码",
  "publicBaseUrl": "http://fluctio:32100",
  "webhookToken": "一段随机字符串",
  "listenPort": "32100"
}
```

| 键 | 说明 |
|---|---|
| `songloftUrl` | Songloft 基地址（从 FastClaw 容器可达，如 `http://songloft:5045`） |
| `songloftUsername` / `songloftPassword` | Songloft 登录账号（调 miot API 要 JWT；token 过期自动刷新/重登） |
| `publicBaseUrl` | 本插件 webhook 监听地址（**从 Songloft 容器可达**，如 `http://fluctio:32100`） |
| `webhookToken` | webhook 鉴权密钥，注册进 Songloft 的 URL 会带 `?token=` |
| `listenHost` / `listenPort` | 监听地址，默认 `0.0.0.0:32100` |
| `maxChars` | TTS 回复截断长度，默认 400（念完即止） |
| `ignoreKeywords` | 逗号分隔的忽略口令（**完全匹配**），默认为播放控制词（下一首/暂停等），避免与 Songloft 语音点歌抢指令 |

启动后插件自动向 Songloft 注册 webhook（幂等；Songloft 未启动则每 60s 重试，
最多 10 次）。

## 已知限制

- **抢答**：说完唤醒词，小爱自己的回复仍会先出声（云端方案通病）；小爱
  会念它自己的答案，然后 agent 的 TTS 才到。本插件不主动 stop（会打断
  正在放的歌）。
- **播放控制交给 Songloft**："下一首/暂停"等口令由 miot 插件处理，本插件
  通过 `ignoreKeywords` 让出这些指令。
- **TTS 需要先见过设备**：`channel.send` 的 TTS 要 `account_id`，来自该设备
  的第一次 webhook。此前 agent 主动推送会失败（还没拿到映射）。
- 对话记录只含用户语音的 ASR 文本，无原始音频。

## 本地测试（无 Songloft）

```sh
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"config":{"webhookToken":"t"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"shutdown"}' \
  | node main.js
# 另一个终端:
curl -s -X POST 'http://localhost:32100/webhook?token=t' \
  -H 'Content-Type: application/json' \
  -d '{"account_id":"a1","device_id":"d1","device_name":"客厅","messages":[{"message":{"timestamp_ms":1,"response":{"answer":[{"question":"你好小爱"}]}}}]}'
```

stdout 应打出 `message.inbound` 通知（channel=xiaoai, chatId=d1, text=你好小爱）。
