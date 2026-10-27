package notify

import (
	"testing"

	"github.com/haihaipypy/autodocker/internal/model"
)

// TargetRef 决定了「点了更新按钮之后到底去拉哪个镜像」。
// 判错就是空操作：跑完一圈，容器还在旧版本上。
func TestTargetRef(t *testing.T) {
	cases := []struct {
		name string
		n    model.Notification
		want string
	}{
		{
			name: "浮动 tag 内容变更：仍拉原引用",
			n: model.Notification{
				ImageRef: "nginx:latest",
				Kind:     model.NotifNewVersion,
			},
			want: "nginx:latest",
		},
		{
			name: "锁定版本发现新 tag：必须换成新版本号",
			n: model.Notification{
				ImageRef:  "nginx:1.25",
				LocalTag:  "1.25",
				LatestTag: "1.27",
				Kind:      model.NotifNewTag,
			},
			want: "nginx:1.27",
		},
		{
			name: "带仓库路径的锁定版本",
			n: model.Notification{
				ImageRef:  "joplin/server:2.13.5",
				LocalTag:  "2.13.5",
				LatestTag: "2.13.6",
				Kind:      model.NotifNewTag,
			},
			want: "joplin/server:2.13.6",
		},
		{
			name: "私有仓库的锁定版本",
			n: model.Notification{
				ImageRef:  "ghcr.io/foo/bar:1.2.3",
				LocalTag:  "1.2.3",
				LatestTag: "1.3.0",
				Kind:      model.NotifNewTag,
			},
			want: "ghcr.io/foo/bar:1.3.0",
		},
		{
			name: "LatestTag 与当前 tag 相同：不折腾，用原引用",
			n: model.Notification{
				ImageRef:  "nginx:1.25",
				LocalTag:  "1.25",
				LatestTag: "1.25",
			},
			want: "nginx:1.25",
		},
		{
			name: "页面直接更新（Kind 未标）也能靠 LatestTag 判出来",
			n: model.Notification{
				ImageRef:  "redis:7.2",
				LocalTag:  "7.2",
				LatestTag: "7.4",
			},
			want: "redis:7.4",
		},
		{
			name: "默认 tag 缺省写法",
			n: model.Notification{
				ImageRef:  "alpine",
				LocalTag:  "latest",
				LatestTag: "3.21",
			},
			want: "alpine:3.21",
		},
		{
			name: "无法解析时保持原样，不做猜测",
			n: model.Notification{
				ImageRef:  "",
				LatestTag: "1.0",
			},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := TargetRef(&c.n)
			if got != c.want {
				t.Fatalf("TargetRef = %q, 期望 %q", got, c.want)
			}
		})
	}
}
