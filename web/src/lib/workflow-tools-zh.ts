// Chinese descriptions for built-in tools, used by the workflow editor when
// the UI locale is zh-CN. MCP / plugin tools always show their upstream
// (English) description — we don't translate those. Missing names fall back
// to the tool's own description. Add entries here as built-in tools ship.
export const BUILTIN_TOOL_ZH: Record<string, string> = {
  get_time: "获取当前时间",
  workflow_resume: "提交 waiting 工作流暂停节点的表单答案，继续运行",
  workflow_list: "列出该 agent 的工作流（id / 版本 / 描述）",
  workflow_get: "读取某个工作流的完整 YAML（编辑前先读，保留现有节点）",
  workflow_save: "创建或更新工作流（upsert：新 id 创建、已有 id 编辑并版本+1，保存即生效）",
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

// Ordered functional groups for the editor's tool dropdown: same-kind tools
// sit together (files together, messaging together, …) instead of one flat
// alphabetical list. Tools not listed (workflow defs, MCP/plugin) fall into a
// trailing bucket, keeping their backend source order.
export const TOOL_GROUPS: { zh: string; en: string; names: string[] }[] = [
  { zh: "时间与偏好", en: "Time & preferences", names: ["get_time", "set_timezone", "set_preference"] },
  { zh: "文件与执行", en: "Files & exec", names: ["read_file", "list_dir", "write_file", "exec", "bash_output", "kill_shell"] },
  { zh: "搜索与知识", en: "Search & knowledge", names: ["web_search", "web_fetch", "memory_search"] },
  { zh: "知识库", en: "Knowledge base", names: ["knowledgebase_search", "knowledgebase_search_raw", "knowledgebase_ingest_url", "knowledgebase_add", "knowledgebase_save_flash", "knowledgebase_save_todo", "knowledgebase_update_todo", "knowledgebase_save_bookmark", "knowledgebase_generate_insights", "knowledgebase_verify_claim", "knowledgebase_list", "knowledgebase_list_flashes", "knowledgebase_list_todos", "knowledgebase_delete"] },
  { zh: "技能", en: "Skills", names: ["load_skill", "search_skills", "skill_manage", "install_skill"] },
  { zh: "消息渠道", en: "Messaging", names: ["message", "list_channels", "fetch_messages"] },
  { zh: "生成与媒体", en: "Media generation", names: ["image_gen", "tts", "vision"] },
  { zh: "定时任务", en: "Cron jobs", names: ["create_cron_job", "list_cron_jobs", "delete_cron_job"] },
  { zh: "子任务与目标", en: "Subagents & goals", names: ["spawn_subagent", "delegate_task", "update_goal"] },
  { zh: "文件编辑与应用", en: "File editing & apps", names: ["edit_file", "apply_patch", "deliver_file", "start_app_preview", "app_preview_logs"] },
  { zh: "工作流管理", en: "Workflow management", names: ["workflow_list", "workflow_get", "workflow_save", "workflow_resume"] },
  { zh: "用量", en: "Usage", names: ["get_billing_usage"] },
];

// groupToolsForDropdown buckets a tool list into the ordered groups above;
// in-group order follows the group's names array. Everything else lands in a
// final "workflows & extensions" bucket in its given (source) order.
export function groupToolsForDropdown<T extends { name: string }>(tools: T[], locale: string): { label: string; tools: T[] }[] {
  const zh = locale !== "en";
  const groups = TOOL_GROUPS.map((g) => ({ label: zh ? g.zh : g.en, names: g.names, tools: [] as T[] }));
  const rest: T[] = [];
  for (const t of tools) {
    const gi = groups.findIndex((g) => g.names.includes(t.name));
    if (gi >= 0) groups[gi].tools.push(t);
    else rest.push(t);
  }
  const out = groups
    .filter((g) => g.tools.length > 0)
    .map((g) => ({
      label: g.label,
      tools: g.tools.sort((a, b) => g.names.indexOf(a.name) - g.names.indexOf(b.name)),
    }));
  if (rest.length > 0) out.push({ label: zh ? "工作流与扩展" : "Workflows & extensions", tools: rest });
  return out;
}
