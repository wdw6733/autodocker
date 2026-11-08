// Package version 提供镜像 tag 的解析、排序与比较。
//
// 目标与 vigil 的 version 包一致：从一堆 tag 里确定性地挑出「版本最高」的那一个。
// 规则要点：
//   - 数字段逐段比较，段数多者视为更具体（1.2.3 > 1.2）；
//   - 相同版本号时，正式版优先于预发布版（1.2.3 > 1.2.3-rc1），
//     预发布版之间再比较序号（rc2 > rc1）；
//   - 非版本号形态（latest、edge、stable 等）不参与版本高低判定。
package version

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	// 抓取 tag 中的数字段，如 v3.9-alpine → [3 9]
	numRe = regexp.MustCompile(`\d+`)
	// 预发布关键词，如 rc / beta / alpha / nightly
	preWordRe = regexp.MustCompile(`(?i)(alpha|beta|rc|pre|dev|snapshot|nightly|canary)`)
)

// parsed 一个 tag 的解析结果。
type parsed struct {
	raw    string
	nums   []int
	pre    string // 预发布关键词（小写），正式版为空
	preNum int
	arch   string // 被剥掉的架构段（386 / arm64…），没有则为空
	suffix string // 版本号之后的额外标识，如 alpine / slim
}

// archWords 已知的 CPU 架构标识。
//
// 有些镜像把 tag 写成「大版本 + 架构」并列发布，例如
// v3 / v3-amd64 / v3-386 / v3-armv7 —— 其中的 386 是 32 位 x86 架构，
// 不是「第 386 版」。不剥掉的话 Parse("v3-386") 会得到数字段 [3 386]，
// 比 Parse("v3-arm64") 的 [3] 大出一截，同一批并列的架构 tag 之间
// 就会冒出「v3-386 有更新」这种假信号。
var archWords = map[string]bool{
	"amd64": true, "x86_64": true, "x64": true, "x86": true,
	"386": true, "i386": true, "i686": true,
	"arm64": true, "aarch64": true, "armv7": true, "armv6": true,
	"armhf": true, "arm": true,
	"ppc64le": true, "s390x": true, "riscv64": true, "mips64le": true, "loong64": true,
}

// stripArchSuffix 剥掉结尾的架构段，返回剩余正文与架构名。
//
// 只认「分隔符 + 架构名」这种结尾形态（v3-386 → v3 / 386）。
// 先按 '-' 找再按 '_' 找，是为了让 x86_64 这种自带下划线的架构名能整体匹配上：
// 对 v1-x86_64 先命中 '-'、尾巴正好是 x86_64；若先按 '_' 切会切成孤零零的 "64"。
func stripArchSuffix(core string) (string, string) {
	for _, sep := range []byte{'-', '_'} {
		i := strings.LastIndexByte(core, sep)
		if i <= 0 || i == len(core)-1 {
			continue
		}
		if tail := core[i+1:]; archWords[tail] {
			return core[:i], tail
		}
	}
	return core, ""
}

// Parse 解析 tag。无法识别出版本号时返回 ok=false。
func Parse(tag string) (p parsed, ok bool) {
	p.raw = tag
	lower := strings.ToLower(strings.TrimSpace(tag))
	if lower == "" {
		return p, false
	}

	// 先把预发布段整个摘出去，再从剩下的正文里抽数字段。
	//
	// 这一步不能省：numRe 会抓走 tag 里**所有**数字串，
	// 若把 "1.2.3-rc1" 整串丢进去，rc 的序号 1 就成了第 4 个版本段，
	// 于是判出「1.2.3-rc1 > 1.2.3」——正式版反倒输给预发布版。
	core, preWord, preNum := splitPrerelease(lower)
	p.pre, p.preNum = preWord, preNum

	// 架构段也要先剥掉再抽数字：v3-386 只该得到 [3]。
	// 顺序在摘预发布之后、抽数字之前 —— 架构名里没有数字以外的干扰（386 除外），
	// 但它同样会被 numRe 抓成版本段。
	core, p.arch = stripArchSuffix(core)

	for _, s := range numRe.FindAllString(core, -1) {
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		p.nums = append(p.nums, n)
	}
	if len(p.nums) == 0 {
		return p, false
	}
	// 版本段之后的后缀（如 -alpine、-slim），用于同版本号时区分
	head := strings.Join(itoa(p.nums), ".")
	if idx := strings.Index(core, head); idx >= 0 {
		p.suffix = strings.Trim(core[idx+len(head):], "-_. ")
	}
	return p, true
}

// splitPrerelease 拆出预发布标识，返回去掉该段后的正文、关键词与序号。
//
// 序号有两种写法都要认：紧跟关键词（rc1）和隔一个分隔符（alpha.2）。
func splitPrerelease(lower string) (core, word string, num int) {
	loc := preWordRe.FindStringSubmatchIndex(lower)
	if loc == nil {
		return lower, "", 0
	}

	start := loc[0]
	// 关键词前面紧挨着的分隔符属于这一段，一并摘掉，
	// 否则 "1.2.3-rc1" 会留下 "1.2.3-" 这种带尾巴的正文
	if start > 0 && isSep(lower[start-1]) {
		start--
	}

	end := loc[1]
	// 序号可能在分隔符之后：alpha.2
	if probe := end; probe < len(lower) && isSep(lower[probe]) {
		probe++
		if probe < len(lower) && isDigit(lower[probe]) {
			numStart := probe
			for probe < len(lower) && isDigit(lower[probe]) {
				probe++
			}
			num, _ = strconv.Atoi(lower[numStart:probe])
			end = probe
		}
	} else {
		// 也可能直接贴着：rc1
		probe := end
		for probe < len(lower) && isDigit(lower[probe]) {
			probe++
		}
		if probe > end {
			num, _ = strconv.Atoi(lower[end:probe])
			end = probe
		}
	}

	return lower[:start] + lower[end:], lower[loc[2]:loc[3]], num
}

func isSep(c byte) bool {
	return c == '-' || c == '_' || c == '.'
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func itoa(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// IsVersionTag 判断一个 tag 是否属于版本号形态。
func IsVersionTag(tag string) bool {
	_, ok := Parse(tag)
	return ok
}

// Compare 比较两个 tag 的版本高低。
// 返回 -1 表示 a < b，0 表示相等，1 表示 a > b。
// 任一方无法解析为版本号时按字符串比较兜底。
func Compare(a, b string) int {
	pa, oka := Parse(a)
	pb, okb := Parse(b)
	if !oka || !okb {
		return strings.Compare(a, b)
	}
	// 数字段逐位比较
	n := len(pa.nums)
	if len(pb.nums) > n {
		n = len(pb.nums)
	}
	for i := 0; i < n; i++ {
		va, vb := 0, 0
		if i < len(pa.nums) {
			va = pa.nums[i]
		}
		if i < len(pb.nums) {
			vb = pb.nums[i]
		}
		if va != vb {
			if va < vb {
				return -1
			}
			return 1
		}
	}
	// 数字段相同：正式版 > 预发布版
	if pa.pre == "" && pb.pre != "" {
		return 1
	}
	if pa.pre != "" && pb.pre == "" {
		return -1
	}
	if pa.pre != pb.pre {
		return strings.Compare(pa.pre, pb.pre)
	}
	if pa.preNum != pb.preNum {
		if pa.preNum < pb.preNum {
			return -1
		}
		return 1
	}
	// 同版本号时，无后缀者视为更「正式」；同后缀再比字符串
	if pa.suffix != pb.suffix {
		if pa.suffix == "" {
			return 1
		}
		if pb.suffix == "" {
			return -1
		}
		return strings.Compare(pa.suffix, pb.suffix)
	}
	// 架构（arch）刻意不参与高低判定：v3-386 与 v3-arm64 是并列发布的两条线，
	// 谁新谁旧得看镜像摘要，版本号回答不了。剥掉它只影响「数字段别被污染」，
	// 不能反过来让某条架构线显得比另一条「更新」。
	return 0
}

// SortTags 返回按版本从高到低排序的 tag 列表（只含版本号形态的 tag）。
func SortTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if IsVersionTag(t) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return Compare(out[i], out[j]) > 0
	})
	return out
}

// BestTag 从候选里挑出版本最高的 tag。没有版本号形态的 tag 时返回空串。
func BestTag(tags []string) string {
	sorted := SortTags(tags)
	if len(sorted) == 0 {
		return ""
	}
	return sorted[0]
}

// NewerThan 返回 tags 中所有严格高于 base 的版本 tag，从高到低排列。
func NewerThan(base string, tags []string) []string {
	sorted := SortTags(tags)
	out := make([]string, 0, len(sorted))
	for _, t := range sorted {
		if Compare(t, base) > 0 {
			out = append(out, t)
		}
	}
	return out
}

// SameVersion 判断两个 tag 是否属于同一版本（忽略后缀差异）。
func SameVersion(a, b string) bool {
	pa, oka := Parse(a)
	pb, okb := Parse(b)
	if !oka || !okb {
		return false
	}
	if len(pa.nums) != len(pb.nums) {
		return false
	}
	for i := range pa.nums {
		if pa.nums[i] != pb.nums[i] {
			return false
		}
	}
	return pa.pre == pb.pre && pa.preNum == pb.preNum
}

// ShortDigest 把 sha256:abcdef... 截断成适合卡片展示的短摘要。
func ShortDigest(d string) string {
	if d == "" {
		return ""
	}
	_, hex, ok := strings.Cut(d, ":")
	if !ok {
		hex = d
	}
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}
