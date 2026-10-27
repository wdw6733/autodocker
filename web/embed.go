// Package web 内嵌前端静态资源，使整个服务可以单二进制方式部署。
package web

import "embed"

// Files 前端资源：一个单页应用，无需构建步骤。
//
// 注意：这里列的是**文件名清单**，不是目录。新增静态资源（例如 favicon.ico）
// 必须同步加进来，否则文件在磁盘上存在、编出来的二进制里却没有，
// 表现为线上 404、而本机拿 StaticDir 指向磁盘时一切正常 —— 极难排查。
//
//go:embed index.html app.js style.css favicon.ico icon-192.png
var Files embed.FS
