// Package migrations 内嵌 SQL 迁移文件，供 infra/db 的执行器与 migrate CLI 读取。
//
// 命名约定：<version>[_<name>].sql 是方言无关主体，<version>_<name>.<dialect>.sql
// 是同一版本的方言覆盖（sqlite / pg），由执行器在主体之后、同一事务内应用。
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed *.sql
var embedded embed.FS

// FS 返回内嵌的迁移文件系统。
func FS() fs.FS { return embedded }
