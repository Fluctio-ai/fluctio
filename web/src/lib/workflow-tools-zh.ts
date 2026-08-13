// Chinese descriptions for built-in tools, used by the workflow editor when
// the UI locale is zh-CN. MCP / plugin tools always show their upstream
// (English) description — we don't translate those. Missing names fall back
// to the tool's own description. Add entries here as built-in tools ship.
export const BUILTIN_TOOL_ZH: Record<string, string> = {
  get_time: "获取当前时间",
  web_search: "搜索网络，返回标题 / URL / 摘要（provider 链 + 自动降级）",
  web_fetch: "抓取网页内容（provider 链：直连 / jina / firecrawl）",
  memory_search: "搜索记忆 / 知识库（向量 + FTS）",
  read_file: "读取文件内容",
  list_dir: "列出目录下的文件与子目录",
  write_file: "写入文件",
  exec: "执行 shell 命令，返回 stdout / stderr（二进制输出请写入工作区文件）",
  image_gen: "文本生成图片，保存到工作区（provider 链：OpenAI / fal）",
  tts: "文本转语音，音频附到消息（provider 链：OpenAI / MiniMax）",
  vision: "图像识别 / 分析",
  message: "发送纯文本消息到指定渠道（不能发图/文件）",
  update_goal: "更新目标状态",
  load_skill: "加载技能的完整内容",
  spawn_subagent: "派生子 agent 执行子任务并返回结果",
  delegate_task: "委派任务给子 agent",
  set_preference: "设置用户偏好",
  set_timezone: "设置时区",
  list_channels: "列出当前可用的渠道",
  fetch_messages: "获取渠道的历史消息",
  create_cron_job: "创建定时任务",
  list_cron_jobs: "列出定时任务",
  delete_cron_job: "删除定时任务",
  bash_output: "读取后台 shell 的输出",
  kill_shell: "终止后台 shell",
  get_billing_usage: "获取用量统计",
};
