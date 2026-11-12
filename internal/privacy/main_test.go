package privacy

import (
	"os"
	"testing"
)

// TestMain 把 FLUCTIO_HOME 指到临时目录：指纹密钥文件
// (privacy.key) 写入隔离的 home，不污染真实安装；多轮 scrub 的
// 确定性只依赖同一进程内密钥一致，与 home 位置无关。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "fluctio-privacy-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("FLUCTIO_HOME", dir)
	os.Exit(m.Run())
}
