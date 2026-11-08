package version

import (
	"reflect"
	"testing"
)

func TestIsVersionTag(t *testing.T) {
	yes := []string{"1.25", "v1.2.3", "3.9-alpine", "2.13.5", "1.2.3-rc1", "2024.10.1", "1.2.3-alpha.2"}
	no := []string{"latest", "stable", "edge", "alpine", "main", "", "nightly"}

	for _, tag := range yes {
		if !IsVersionTag(tag) {
			t.Errorf("IsVersionTag(%q) 应为 true", tag)
		}
	}
	for _, tag := range no {
		if IsVersionTag(tag) {
			t.Errorf("IsVersionTag(%q) 应为 false", tag)
		}
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.25", "1.27", -1},
		{"1.27", "1.25", 1},
		{"1.2.3", "1.2.3", 0},
		// 正式版高于预发布版
		{"1.2.3", "1.2.3-rc1", 1},
		{"1.2.3-rc1", "1.2.3", -1},
		{"1.2.3-rc2", "1.2.3-rc1", 1},
		// 隔一个分隔符写序号的形态（alpha.2）
		{"1.2.3-alpha.2", "1.2.3-alpha.1", 1},
		{"1.2.3-alpha.2", "1.2.3-alpha.10", -1},
		// 预发布段之后还挂后缀时，序号不能渗进版本段里
		{"1.2.3-rc1-alpine", "1.2.3-alpine", -1},
		{"1.2.3-rc1-alpine", "1.2.4-alpine", -1},
		// 同为预发布，按关键词排序
		{"1.2.3-beta", "1.2.3-rc1", -1},
		// 段数多者更具体：1.2.3 比 1.2 高，因为缺的段按 0 补
		{"1.2.3", "1.2", 1},
		{"1.2", "1.2.3", -1},
		{"1.2.1", "1.2", 1},
		// 1.2 与 1.2.0 是同一个版本：缺的段按 0 补，不能互相判成「有更新」
		{"1.2", "1.2.0", 0},
		{"1.2.0", "1.2", 0},
		// 同版本号时，无后缀者更正式
		{"1.2.3", "1.2.3-alpine", 1},
		{"1.2.3-slim", "1.2.3-alpine", 1},
		// 大版本跨越
		{"2.0", "10.0", -1},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, 期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewerThan(t *testing.T) {
	tags := []string{"1.24", "1.26", "1.27", "latest", "edge", "1.25"}
	got := NewerThan("1.25", tags)
	want := []string{"1.27", "1.26"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NewerThan = %v, 期望 %v（latest/edge 不参与版本高低判定）", got, want)
	}

	// 已经是最新：不应提示任何更新
	if got := NewerThan("1.27", tags); len(got) != 0 {
		t.Fatalf("NewerThan 应为空，得到 %v", got)
	}
}

func TestSortTagsDescending(t *testing.T) {
	got := SortTags([]string{"1.9", "1.10", "1.2", "latest", "2.0-rc1"})
	want := []string{"2.0-rc1", "1.10", "1.9", "1.2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SortTags = %v, 期望 %v（注意 1.10 > 1.9，不能按字符串排）", got, want)
	}
}

func TestShortDigest(t *testing.T) {
	if got := ShortDigest("sha256:abcdef0123456789"); got != "abcdef012345" {
		t.Fatalf("ShortDigest = %q", got)
	}
	if got := ShortDigest(""); got != "" {
		t.Fatalf("ShortDigest(\"\") = %q", got)
	}
}

// 「大版本 + 架构」这类 tag 里的数字是架构号，不是版本号。
// 剥不剥掉，直接决定同一批架构 tag 会不会互相报「有更新」。
func TestParseStripsArchSuffix(t *testing.T) {
	cases := []struct {
		tag    string
		nums   []int
		arch   string
		suffix string
	}{
		{"v3-386", []int{3}, "386", ""},
		{"v3-amd64", []int{3}, "amd64", ""},
		{"v3-armv7", []int{3}, "armv7", ""},
		{"v1-x86_64", []int{1}, "x86_64", ""},
		{"v3_xiaojv_waf-386", []int{3}, "386", "xiaojv_waf"},
		// 不能误伤：这些后缀不是架构名，照旧当成后缀
		{"1.2.3-alpine", []int{1, 2, 3}, "", "alpine"},
		{"1.2.3-slim", []int{1, 2, 3}, "", "slim"},
		{"1.2.3", []int{1, 2, 3}, "", ""},
	}
	for _, c := range cases {
		p, ok := Parse(c.tag)
		if !ok {
			t.Errorf("Parse(%q) 失败", c.tag)
			continue
		}
		if !reflect.DeepEqual(p.nums, c.nums) {
			t.Errorf("Parse(%q).nums = %v, 期望 %v", c.tag, p.nums, c.nums)
		}
		if p.arch != c.arch {
			t.Errorf("Parse(%q).arch = %q, 期望 %q", c.tag, p.arch, c.arch)
		}
		if p.suffix != c.suffix {
			t.Errorf("Parse(%q).suffix = %q, 期望 %q", c.tag, p.suffix, c.suffix)
		}
	}
}

// 并列发布的架构 tag 之间不该比出高低；真正的版本号递增仍要认出来。
func TestArchTagsArePeers(t *testing.T) {
	peers := [][2]string{
		{"v3-386", "v3-arm64"},
		{"v3-amd64", "v3-armv7"},
		{"v3-386", "v3"}, // 裸 v3 与 v3-386 同属 v3 系列
	}
	for _, p := range peers {
		if got := Compare(p[0], p[1]); got != 0 {
			t.Errorf("Compare(%q, %q) = %d, 期望 0（并列发布，不该比出高低）", p[0], p[1], got)
		}
	}

	// 反向：真的出现更高的大版本时要认得出来，否则修完就瞎了
	if got := Compare("v3-386", "v4"); got != -1 {
		t.Errorf("Compare(v3-386, v4) = %d, 期望 -1（v4 是更高的版本）", got)
	}
	if got := Compare("v3-386", "3.5.0"); got != -1 {
		t.Errorf("Compare(v3-386, 3.5.0) = %d, 期望 -1（3.5.0 的次版本更高）", got)
	}

	// 误伤检查：带后缀的同版本号之间仍按原规则比（无后缀者更「正式」）
	if got := Compare("1.2.3", "1.2.3-alpine"); got != 1 {
		t.Errorf("Compare(1.2.3, 1.2.3-alpine) = %d, 期望 1", got)
	}
}

// 实际场景：一条 v3-386 的部署，去仓库里找「更高版本」，不该把并列的架构 tag 算进来。
func TestNewerThanIgnoresSiblingArchTags(t *testing.T) {
	tags := []string{"latest", "v3", "v3-amd64", "v3-arm64", "v3-armv7", "v3-386", "3.0.0"}
	if got := NewerThan("v3-386", tags); len(got) != 0 {
		t.Fatalf("NewerThan(v3-386) = %v, 期望空（这批 tag 里没有更高的版本号）", got)
	}

	// 仓库真出了 v4 系列时要能报出来
	tags = append(tags, "v4", "v4-amd64")
	got := NewerThan("v3-386", tags)
	if len(got) == 0 || got[0] != "v4" {
		t.Fatalf("NewerThan(v3-386) = %v, 期望最高一个是 v4", got)
	}
}
