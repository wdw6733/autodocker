package updater

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/haihaipypy/autodocker/internal/dockerx"
)

// 这是一份贴近真实 inspect 响应的容器快照：
// 注意 HostConfig["Binds"] 是 []any（JSON 解出来的形态），
// 而匿名卷只会出现在 Mounts 里、不会出现在 Binds 里。
func sampleInspect() *dockerx.Inspect {
	return &dockerx.Inspect{
		ID:    "3f8a1b2c9d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8",
		Name:  "joplin",
		Image: "joplin/server:2.13.5",
		State: &dockerx.State{Running: true, Status: "running"},
		Config: map[string]any{
			"Hostname":     "3f8a1b2c9d4e", // Docker 自动按短 ID 填的
			"Image":        "joplin/server:2.13.5",
			"Env":          []any{"TZ=Asia/Shanghai", "APP_PORT=22300"},
			"User":         "node",
			"WorkingDir":   "/home/node",
			"ExposedPorts": map[string]any{"22300/tcp": map[string]any{}},
		},
		HostConfig: map[string]any{
			"Binds":       []any{"/srv/joplin:/home/node/.config"},
			"NetworkMode": "bridge",
			"AutoRemove":  false,
			"RestartPolicy": map[string]any{
				"Name": "unless-stopped",
			},
		},
		NetworkSettings: &dockerx.NetworkSettings{
			Networks: map[string]*dockerx.EndpointSettings{
				"bridge": {
					NetworkID:  "e1f2a3b4c5d6",
					EndpointID: "9a8b7c6d5e4f",
					Gateway:    "172.17.0.1",
					IPAddress:  "172.17.0.2",
					MacAddress: "02:42:ac:11:00:02",
				},
			},
		},
		Mounts: []dockerx.MountPoint{
			{Type: "bind", Source: "/srv/joplin", Destination: "/home/node/.config"},
			{Type: "volume", Name: "a1b2c3d4e5f6", Destination: "/var/lib/postgresql/data"},
		},
	}
}

// 最要命的一处：Config.Image 必须被改成新引用。
// 否则「更高版本 tag」场景会重建出一个依旧跑着旧镜像的容器——表面成功、实际没更新。
func TestBuildSpecOverridesImage(t *testing.T) {
	cfg, _, _, _ := buildSpec(sampleInspect(), "joplin/server:2.13.6")

	if got := cfg["Image"]; got != "joplin/server:2.13.6" {
		t.Fatalf("Config.Image 未被覆盖为本次目标引用，得到 %v", got)
	}
	// 其余配置不能被顺手改掉
	if cfg["User"] != "node" || cfg["WorkingDir"] != "/home/node" {
		t.Fatalf("克隆过程中丢失了原有 Config 字段: %#v", cfg)
	}
	if _, ok := cfg["Env"]; !ok {
		t.Fatal("Env 丢失")
	}
}

// Hostname 默认是旧容器的短 ID，照抄会把它带进新容器；但用户手写的必须保留。
func TestBuildSpecHostname(t *testing.T) {
	insp := sampleInspect()
	cfg, _, _, _ := buildSpec(insp, insp.Image)
	if _, ok := cfg["Hostname"]; ok {
		t.Fatalf("自动分配的 Hostname 应被清掉，实际仍为 %v", cfg["Hostname"])
	}

	insp2 := sampleInspect()
	insp2.Config["Hostname"] = "joplin-web" // 用户显式 --hostname
	cfg2, _, _, _ := buildSpec(insp2, insp2.Image)
	if cfg2["Hostname"] != "joplin-web" {
		t.Fatalf("用户显式设置的 Hostname 不应被清掉，得到 %v", cfg2["Hostname"])
	}
}

// 匿名卷（只在 Mounts 里、不在 Binds 里）必须补成显式绑定，否则重建后数据落到新空卷。
// 已经覆盖的目标不能重复添加 —— 重复挂载点会让创建直接失败。
func TestBuildSpecAnonymousVolume(t *testing.T) {
	_, hostCfg, _, _ := buildSpec(sampleInspect(), "joplin/server:2.13.6")

	binds := toStringSlice(hostCfg["Binds"])
	joined := strings.Join(binds, " ")

	if !strings.Contains(joined, "a1b2c3d4e5f6:/var/lib/postgresql/data") {
		t.Fatalf("匿名卷未被补回 Binds: %v", binds)
	}
	if !strings.Contains(joined, "/srv/joplin:/home/node/.config") {
		t.Fatalf("原有 bind 挂载丢失: %v", binds)
	}
	// /home/node/.config 已由 Binds 覆盖，不应再出现第二次
	if n := strings.Count(joined, "/home/node/.config"); n != 1 {
		t.Fatalf("/home/node/.config 出现 %d 次，会导致 duplicate mount point: %v", n, binds)
	}
	if n := strings.Count(joined, "/var/lib/postgresql/data"); n != 1 {
		t.Fatalf("/var/lib/postgresql/data 出现 %d 次: %v", n, binds)
	}
}

// 内置网络不能塞进 EndpointsConfig —— 这是 host 网络容器更新失败最常见的原因。
func TestBuildSpecBuiltinNetworkExcluded(t *testing.T) {
	insp := sampleInspect()
	insp.HostConfig["NetworkMode"] = "host"
	insp.NetworkSettings.Networks = map[string]*dockerx.EndpointSettings{
		"host": {NetworkID: "abc", EndpointID: "def", IPAddress: "172.17.0.2"},
	}

	_, _, netCfg, warns := buildSpec(insp, insp.Image)
	if netCfg != nil {
		t.Fatalf("host 网络不应生成 NetworkingConfig，得到 %#v", netCfg)
	}
	if !warnsContain(warns, "host 网络") {
		t.Fatalf("应提示 host 网络端口短暂不可用，得到 %v", warns)
	}
}

// 用户自定义网络要保留，但运行时字段必须剔除。
func TestBuildSpecUserNetworkKeepsEndpoint(t *testing.T) {
	insp := sampleInspect()
	insp.HostConfig["NetworkMode"] = "mynet"
	insp.NetworkSettings.Networks = map[string]*dockerx.EndpointSettings{
		"mynet": {
			NetworkID:  "nid",
			EndpointID: "eid",
			Gateway:    "10.0.0.1",
			IPAddress:  "10.0.0.8",
			MacAddress: "02:42:0a:00:00:08",
			Aliases:    []string{"joplin", "notes"},
		},
		"bridge": {NetworkID: "bid", IPAddress: "172.17.0.3"},
	}

	_, _, netCfg, _ := buildSpec(insp, insp.Image)
	if netCfg == nil {
		t.Fatal("自定义网络应生成 NetworkingConfig")
	}
	eps, ok := netCfg["EndpointsConfig"].(map[string]any)
	if !ok {
		t.Fatalf("EndpointsConfig 类型异常: %#v", netCfg)
	}
	if _, hasBridge := eps["bridge"]; hasBridge {
		t.Fatal("内置 bridge 网络不应出现在 EndpointsConfig 里")
	}
	entry, hasMynet := eps["mynet"].(map[string]any)
	if !hasMynet {
		t.Fatalf("自定义网络 mynet 丢失: %#v", eps)
	}

	// 运行时字段一个都不能漏出去
	blob, _ := json.Marshal(entry)
	for _, banned := range []string{"NetworkID", "EndpointID", "Gateway", "IPAddress", "MacAddress"} {
		if strings.Contains(string(blob), banned) {
			t.Fatalf("端点设置里混进了运行时字段 %s: %s", banned, blob)
		}
	}
	if !strings.Contains(string(blob), "joplin") {
		t.Fatalf("别名应被保留: %s", blob)
	}
}

// 共享其它容器网络栈时不传任何端点。
func TestBuildSpecContainerNetworkMode(t *testing.T) {
	insp := sampleInspect()
	insp.HostConfig["NetworkMode"] = "container:abc123"
	_, _, netCfg, _ := buildSpec(insp, insp.Image)
	if netCfg != nil {
		t.Fatalf("container: 模式不应生成 NetworkingConfig，得到 %#v", netCfg)
	}
}

// bindDestination 要能应付各种 -v 写法，否则覆盖判断会漏、补出重复挂载点。
func TestBindDestination(t *testing.T) {
	cases := map[string]string{
		"/data":                      "/data",
		"vol:/data":                  "/data",
		"vol:/data:ro":               "/data",
		"vol:/data:rw":               "/data",
		"vol:/data:ro,z":             "/data",
		"vol:/var/lib/app:cached":    "/var/lib/app",
		"vol:/var/lib/app:rprivate":  "/var/lib/app",
		`C:\Users\akira\data:/data`:  "/data",
		`C:\Users\akira\data:/d:ro`:  "/d",
		"/host/path:/container/path": "/container/path",
		"":                           "",
	}
	for in, want := range cases {
		if got := bindDestination(in); got != want {
			t.Errorf("bindDestination(%q) = %q, 期望 %q", in, got, want)
		}
	}
}

func warnsContain(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
