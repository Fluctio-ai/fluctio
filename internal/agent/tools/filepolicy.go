package tools

import (
	"path/filepath"
	"slices"
	"strings"
)

type FileCategory string

const (
	CategoryIdentity FileCategory = "identity" // SOUL, IDENTITY, agent.json — agent 是什么
	CategoryScaffold FileCategory = "scaffold" // AGENTS, BOOTSTRAP, HEARTBEAT, TOOLS — agent loop 脚手架
	CategoryPerUser  FileCategory = "peruser"  // USER, MEMORY — per-chatter 数据
)

type ReadScope string

const (
	ScopeOwner   ReadScope = "owner"   // overlay：chatter 经 owner-row fallback 继承
	ScopeChatter ReadScope = "chatter" // exact：per-chatter 独立行，新访客见空
)

type WriteActor string

const (
	ActorOwner   WriteActor = "owner"
	ActorChatter WriteActor = "chatter"
	ActorReview  WriteActor = "review" // 阶段 2 后台审查；阶段 1 仅定义常量，不加入任何文件的 WritableBy
)

// FilePolicy 声明一个受管文件的类别、读取作用域与可写主体。
type FilePolicy struct {
	Name       string
	Category   FileCategory
	ReadScope  ReadScope
	WritableBy []WriteActor
}

// filePolicies 是 agent 受管文件的唯一权威清单。
// 改一处文件分类只改这里；三个消费点（读取作用域 / 写入权限 / 写入路由）全部查表。
var filePolicies = []FilePolicy{
	{"SOUL.md", CategoryIdentity, ScopeOwner, []WriteActor{ActorOwner}},
	{"IDENTITY.md", CategoryIdentity, ScopeOwner, []WriteActor{ActorOwner}},
	{"agent.json", CategoryIdentity, ScopeOwner, []WriteActor{ActorOwner}},
	{"AGENTS.md", CategoryScaffold, ScopeOwner, []WriteActor{ActorOwner}},
	{"BOOTSTRAP.md", CategoryScaffold, ScopeOwner, []WriteActor{ActorOwner}},
	{"HEARTBEAT.md", CategoryScaffold, ScopeOwner, []WriteActor{ActorOwner}},
	{"TOOLS.md", CategoryScaffold, ScopeOwner, []WriteActor{ActorOwner}},
	{"USER.md", CategoryPerUser, ScopeChatter, []WriteActor{ActorOwner, ActorChatter, ActorReview}},
	{"MEMORY.md", CategoryPerUser, ScopeChatter, []WriteActor{ActorOwner, ActorChatter, ActorReview}},
}

// PolicyFor 按文件名查策略。未命中返回 (_, false)。
func PolicyFor(name string) (FilePolicy, bool) {
	for _, p := range filePolicies {
		if p.Name == name {
			return p, true
		}
	}
	return FilePolicy{}, false
}

// ManagedFileBase 判定 path 是否指向受管文件，返回 basename 与是否受管。
// 路径形态判定（原 isIdentityFilePath 的语义，必须保留）：
//   - bare basename（"SOUL.md"）→ 受管
//   - 绝对路径（basename 是受管文件，含 Windows 盘符/UNC 与 Unix /）→ 受管
//   - 嵌套相对路径（"notes/SOUL.md"）→ 不受管（chatter 工作区文件，同名巧合）
func ManagedFileBase(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if _, ok := PolicyFor(base); !ok {
		return "", false
	}
	if filepath.IsAbs(path) || strings.HasPrefix(filepath.ToSlash(path), "/") {
		return base, true // 绝对路径 → 受管
	}
	if strings.ContainsRune(clean, filepath.Separator) {
		return "", false // 嵌套相对路径 → 不受管（chatter 工作区文件，同名巧合）
	}
	return base, true // bare basename → 受管
}

// WriteAllowed 判定 actor 是否可访问 path。非受管文件（普通工作区文件）一律放行。
func WriteAllowed(path string, actor WriteActor) bool {
	base, managed := ManagedFileBase(path)
	if !managed {
		return true
	}
	p, _ := PolicyFor(base)
	return slices.Contains(p.WritableBy, actor)
}

// IsChatterScoped 报告 name 是否 per-chatter 作用域（读取走 Exact，不继承 owner 行）。
// 供 context.loadFileForUser 决定走 GetWorkspaceFileExact 还是 GetWorkspaceFile。
func IsChatterScoped(name string) bool {
	if p, ok := PolicyFor(name); ok {
		return p.ReadScope == ScopeChatter
	}
	return false
}
