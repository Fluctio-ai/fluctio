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
var (
	installKey  = sync.OnceValue(loadInstallKey)
	fallbackKey = sync.OnceValue(func() []byte {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			// 最后的兜底: 全零密钥 (HMAC 仍可计算, 只是无密钥隔离)。
			clear(k)
		}
		return k
	})
)

// loadInstallKey 读或建 $FLUCTIO_HOME/privacy.key。home 不可用时退化
// 为进程内临时密钥 (功能可用, 跨重启不稳定); 写盘失败同理 (记日志)。
func loadInstallKey() []byte {
	home, err := config.HomeDir()
	if err != nil || home == "" {
		return fallbackKey()
	}
	path := filepath.Join(home, "privacy.key")
	if data, err := os.ReadFile(path); err == nil {
		if k, err := hex.DecodeString(string(data)); err == nil && len(k) == 32 {
			return k
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return fallbackKey()
	}
	if err := os.MkdirAll(home, 0o700); err == nil {
		if err := os.WriteFile(path, []byte(hex.EncodeToString(k)), 0o600); err != nil {
			log.Printf("privacy: 写入 %s 失败, 本次进程使用临时指纹密钥: %v", path, err)
			return k
		}
	}
	return k
}

// KeyFor 是 agent 维度的指纹密钥: HMAC(安装密钥, agentID)。
// agentID 为空 = 安装级全局密钥 (无 agent 上下文的调用点: 自由函数
// Scrub/ContainsPII、测试)。
func KeyFor(agentID string) []byte {
	h := hmac.New(sha256.New, installKey())
	h.Write([]byte(agentID))
	return h.Sum(nil)
}

// keyOrDefault 是 opts.Key 为空时的安装级全局密钥。
func (o Options) keyOrDefault() []byte {
	if len(o.Key) > 0 {
		return o.Key
	}
	return installKey()
}
