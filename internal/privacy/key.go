package privacy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/fluctio-ai/fluctio/internal/config"
)

// 指纹密钥体系: 假名占位符的指纹 = HMAC(密钥, 原值) 截断。密钥两层:
//
//   - 安装级随机密钥: 持久化在 $FLUCTIO_HOME/privacy.key (0600)。防
//     离线枚举——拿不到该文件, 对手机号全域枚举也匹配不上指纹
//     (裸 sha256 前缀做不到这一点)。
//   - agent 维度混入: KeyFor(agentID) = HMAC(安装密钥, agentID)。
//     同一安装内不同 agent 对同一值生成不同指纹, 厂商无法跨 agent
//     关联同一 PII; 跨重启稳定 (安装密钥 + agentID 都持久)。
//
// 轮换代价: 删除 privacy.key 后旧历史的旧占位符指纹不再重现 (新一轮
// scrub 换新指纹), 旧回复里的占位符也失去恢复来源。与 magpie 的
// per-install key() 同理。
var secretCache = struct {
	sync.Mutex
	byHome map[string][]byte
}{byHome: map[string][]byte{}}

// installSecret 是 home 的安装级密钥 (32 字节), 按 home 路径缓存。
// 文件缺失/不可写时退化为进程内临时密钥 (功能可用, 跨重启不稳定,
// 并记日志)。
func installSecret() []byte {
	home, err := config.HomeDir()
	if err != nil || home == "" {
		return ephemeralSecret("(no home)")
	}
	secretCache.Lock()
	defer secretCache.Unlock()
	if k, ok := secretCache.byHome[home]; ok {
		return k
	}
	k := loadOrCreateSecret(home)
	secretCache.byHome[home] = k
	return k
}

func loadOrCreateSecret(home string) []byte {
	path := filepath.Join(home, "privacy.key")
	if data, err := os.ReadFile(path); err == nil {
		if k, err := hex.DecodeString(string(data)); err == nil && len(k) == 32 {
			return k
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		// 系统 RNG 失败: 退化为进程临时密钥, 至少功能可用。
		return ephemeralSecret("rand failed")
	}
	if err := os.MkdirAll(home, 0o700); err == nil {
		if err := os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600); err != nil {
			log.Printf("privacy: 写入 %s 失败, 本次进程使用临时指纹密钥: %v", path, err)
			return k
		}
	}
	return k
}

// ephemeral 进程的兜底密钥 (无 home / RNG 失败时用, 不落盘)。
var ephemeral struct {
	sync.Mutex
	byWhy map[string][]byte
}

func ephemeralSecret(why string) []byte {
	ephemeral.Lock()
	defer ephemeral.Unlock()
	if ephemeral.byWhy == nil {
		ephemeral.byWhy = map[string][]byte{}
	}
	if k, ok := ephemeral.byWhy[why]; ok {
		return k
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		// 最后的兜底: 全零密钥 (HMAC 仍可计算, 只是无密钥隔离)。
		clear(k)
	}
	ephemeral.byWhy[why] = k
	return k
}

// KeyFor 是 agent 维度的指纹密钥: HMAC(安装密钥, agentID)。
// agentID 为空 = 安装级全局密钥 (无 agent 上下文的调用点: 自由函数
// Scrub/ContainsPII、测试)。
func KeyFor(agentID string) []byte {
	h := hmac.New(sha256.New, installSecret())
	h.Write([]byte(agentID))
	return h.Sum(nil)
}

// keyOrDefault 是 opts.Key 为空时的安装级全局密钥。
func (o Options) keyOrDefault() []byte {
	if len(o.Key) > 0 {
		return o.Key
	}
	return installSecret()
}
