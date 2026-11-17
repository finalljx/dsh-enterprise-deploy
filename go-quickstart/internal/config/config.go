// Package config 负责把"环境变量"翻译成结构体。
//
// Go 惯例：配置在 main 入口处读取一次，然后显式往下传，
// 不在业务代码里到处 os.Getenv（那会让测试很难做）。
package config

import "os"

// Config 是应用的全部可配置项。字段首字母大写 = 对包外可见。
type Config struct {
	Addr string // HTTP 监听地址，如 ":8080"
}

// Load 从环境变量读取配置，缺省值在这里兜底。
func Load() Config {
	addr := os.Getenv("APP_ADDR")
	if addr == "" {
		addr = ":8088"
	}
	return Config{Addr: addr}
}
