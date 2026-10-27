// 命令 elfcheck 用 Go 标准库解析一个 ELF 文件，确认它结构完整。
// 放在这里是为了在 Windows 上交叉编译出 Linux 二进制后，能立刻验证产物没被破坏。
package main

import (
	"debug/elf"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: elfcheck <文件>")
		os.Exit(2)
	}
	path := os.Args[1]

	f, err := elf.Open(path)
	if err != nil {
		fmt.Printf("FAIL 无法解析: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	info, _ := os.Stat(path)
	fmt.Printf("  OK   类型=%v 机器=%v 入口=0x%x\n", f.Type, f.Machine, f.Entry)
	fmt.Printf("       段数=%d 程序头=%d 大小=%d\n", len(f.Sections), len(f.Progs), info.Size())

	bad := 0
	for _, s := range f.Sections {
		// SHT_NOBITS 不占文件空间，偏移越界是正常的
		if s.Type == elf.SHT_NOBITS {
			continue
		}
		if int64(s.Offset)+int64(s.Size) > info.Size() {
			fmt.Printf("       越界段 %q off=%d size=%d\n", s.Name, s.Offset, s.Size)
			bad++
		}
	}
	if bad > 0 {
		fmt.Printf("FAIL 有 %d 个段越界\n", bad)
		os.Exit(1)
	}
	fmt.Println("       所有段边界正常")
}
