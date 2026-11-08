package registry

import "testing"

// 回环地址必须自动走 http：Docker 守护进程把 127.0.0.0/8 与 localhost 当安全源，
// 本地/私有注册表大多是明文。这条规则一旦失效，用户就只能去打开全局的
// 「允许 http 明文注册表」，连带把 Docker Hub 的检测一起拖成「未知」。
func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:5000", true},
		{"127.0.0.1", true},
		{"127.1.2.3:5000", true}, // 整个 127.0.0.0/8 都是回环
		{"localhost:5000", true},
		{"localhost", true},
		{"LOCALHOST:5000", true},
		{"[::1]:5000", true},
		{"::1", true},

		// 以下都不该被误判成回环
		{"", false},
		{"registry-1.docker.io", false},
		{"docker.m.daocloud.io", false},
		{"ghcr.io", false},
		{"myregistry:5000", false},
		{"192.168.1.10:5000", false}, // 私有网段：Docker 也不认，别自作主张
		{"10.0.0.5:5000", false},
	}
	for _, c := range cases {
		if got := isLoopbackHost(c.host); got != c.want {
			t.Errorf("isLoopbackHost(%q) = %v, 期望 %v", c.host, got, c.want)
		}
	}
}

// endpoint 的 scheme 选择：回环地址无需任何开关即走 http；
// 其余主机默认 https，只有显式打开 Insecure 才整体降级为 http。
func TestEndpointScheme(t *testing.T) {
	plain := New(Options{})

	if got := plain.endpoint("127.0.0.1:5000", "/v2/"); got != "http://127.0.0.1:5000/v2/" {
		t.Errorf("回环地址应走 http，得到 %q", got)
	}
	if got := plain.endpoint("localhost:5000", "/v2/"); got != "http://localhost:5000/v2/" {
		t.Errorf("localhost 应走 http，得到 %q", got)
	}
	if got := plain.endpoint("registry-1.docker.io", "/v2/"); got != "https://registry-1.docker.io/v2/" {
		t.Errorf("Docker Hub 应保持 https，得到 %q", got)
	}
	if got := plain.endpoint("docker.m.daocloud.io", "/v2/"); got != "https://docker.m.daocloud.io/v2/" {
		t.Errorf("加速站应保持 https，得到 %q", got)
	}
	if got := plain.endpoint("192.168.1.10:5000", "/v2/"); got != "https://192.168.1.10:5000/v2/" {
		t.Errorf("非回环的私有地址应保持 https（仍需手动开关），得到 %q", got)
	}

	// 显式打开开关：连 Docker Hub 也一起降级，这是语义本身，不是 bug
	insecure := New(Options{Insecure: true})
	if got := insecure.endpoint("registry-1.docker.io", "/v2/"); got != "http://registry-1.docker.io/v2/" {
		t.Errorf("开启 Insecure 后应走 http，得到 %q", got)
	}
}
