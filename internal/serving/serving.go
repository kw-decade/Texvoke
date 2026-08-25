// Package serving 装两个部署形态（cmd/utr-server 与 cmd/texvoke）都需要的
// 服务端判据与状态。
//
// 为什么共享而不是各写一份：这里放的是**判据**，不是业务逻辑。同一个监听
// 地址在两个二进制里必须得到同一个答案，同一个会话在两处必须爬同一级阶梯。
// 判据分成两份的代价不是多几十行，而是修好一处、另一处照旧——本项目在
// 空白定义上就吃过这个亏（两个判据对同一行给出相反答案）。审计已经抓到
// 一次实际漂移：sidecar 的会话库在键空间超限时连活跃会话一起清，
// gateway 的不清。
//
// 不放进来的东西：HTTP 路由、编排循环、渲染。那些是各自二进制的形态差异，
// sidecar 暴露零件（compile/parse/adapt/recover/render），texvoke 暴露成品
// （三协议端点），强行统一只会把两个能独立演化的形态耦死。
package serving

import (
	"net"
	"strings"
)

// LoopbackOnly 判断这个监听地址是否只对本机可见。
//
// 空 host——也就是 ":8757" 这种最常见的简写——必须算**非**环回：
// http.Server 会把它绑到所有网卡，与 "0.0.0.0:8757" 完全等价。把它当环回
// 是这道防护栏最容易被绕过的方式，因为绝大多数人写监听地址时省掉的正是
// host 那一段。
//
// 无法解析成 IP 的主机名（example.com）一律按非环回处理：这里查不到它会
// 解析到哪，而猜错的方向是把服务暴露出去。
func LoopbackOnly(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// 没有端口分隔符，整串当 host（"localhost"、"::1"）。
		host = strings.Trim(addr, "[]")
	}
	switch host {
	case "":
		return false
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
