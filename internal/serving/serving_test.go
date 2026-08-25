package serving

import "testing"

func TestLoopbackOnly(t *testing.T) {
	loopback := []string{
		"127.0.0.1:8757",
		"localhost:8757",
		"[::1]:8757",
		"127.1.2.3:80", // 127.0.0.0/8 整段都是环回
		"localhost",    // 没写端口
		"::1",
	}
	for _, a := range loopback {
		if !LoopbackOnly(a) {
			t.Errorf("%q 应被判为环回", a)
		}
	}

	remote := []string{
		"0.0.0.0:8757",
		"192.168.1.5:8757",
		"example.com:8757",
		"[::]:8757",
		// 空 host 是这道栏最容易被绕过的写法：http.Server 把它绑到所有
		// 网卡，与 0.0.0.0 等价。旧实现把它判成环回，于是最常见的简写
		// 直接穿过了防护。
		":8757",
		"",
	}
	for _, a := range remote {
		if LoopbackOnly(a) {
			t.Errorf("%q 不该被判为环回——绑到它等于把服务开放给整个网络", a)
		}
	}
}
