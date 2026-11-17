// Package main 演示 Go 闭包，并与 JavaScript 逐条对照。
//
// 运行：
//
//	go run ./examples/closures
//	go run -race ./examples/closures   # 观察并发闭包的数据竞争
//
// 对照的 JS 版本见同目录 demo.mjs（node demo.mjs）。
package main

import (
	"fmt"
	"sync"
)

func main() {
	demoCounter()
	demoCaptureVariable()
	demoCaptureValue()
	demoLoopVar()
	demoGoroutineLoop()
	demoFuncTypeMethod()
	demoMemoize()
	demoRecursive()
	demoDefer()
}

// --- 1. 计数器：闭包持有状态 --------------------------------------------
//
// JS: function newCounter() { let n = 0; return { inc: () => n++, get: () => n } }
// 几乎一模一样。区别只是 Go 用两个函数返回值，JS 用对象包起来。
func demoCounter() {
	inc, get := newCounter()
	inc()
	inc()
	inc()
	fmt.Println("1) 计数器:", get())
}

func newCounter() (inc func(), get func() int) {
	n := 0 // n 逃逸到堆上，被两个闭包共享
	inc = func() { n++ }
	get = func() int { return n }
	return
}

// --- 2. 捕获的是「变量」，不是「值」 ------------------------------------
//
// 两个语言行为完全一致：闭包读的是变量当前的值。
func demoCaptureVariable() {
	x := 1
	f := func() int { return x }
	x = 100
	fmt.Println("2) 捕获变量: f() =", f(), "（外部改成 100 了）")
}

// --- 3. 想捕获「值」：显式拷一份 ----------------------------------------
//
// 经典手法：把值作为参数传进构造函数，闭包捕获参数（参数本身是新的变量）。
func demoCaptureValue() {
	y := 7
	g := captureValue(y)
	y = 999
	fmt.Println("3) 显式捕获值: g() =", g(), "（外部 y 变了也不受影响）")
}

func captureValue(v int) func() int { return func() int { return v } }

// --- 4. 循环变量：Go 1.22 追平 JS 的 let --------------------------------
//
// 把 go.mod 里的 go 指令改成 1.21 再跑，这行会输出 3 3 3（和 JS 的 var 一样）。
// 保持 1.22+ 则输出 0 1 2（和 JS 的 let 一样）。
func demoLoopVar() {
	var fns []func() int
	for i := 0; i < 3; i++ {
		fns = append(fns, func() int { return i })
	}
	fmt.Print("4) 循环变量: ")
	for _, f := range fns {
		fmt.Print(f(), " ")
	}
	fmt.Println()
}

// --- 5. goroutine + 循环变量：形态像 setTimeout，但风险不同 --------------
//
// JS 是单线程事件循环，回调里 push 数组不需要锁。
// Go 是真正并行，多个 goroutine 同时 append 会 data race（加 -race 去掉锁试试）。
func demoGoroutineLoop() {
	var wg sync.WaitGroup
	var mu sync.Mutex

	got := []int{}
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock() // JS 里不需要这行，Go 里不加就是 bug
			got = append(got, i)
			mu.Unlock()
		}()
	}
	wg.Wait()
	fmt.Println("5) 三个 goroutine 拿到的 i:", got, "（顺序不定）")
}

// --- 6. 给函数类型加方法：JS 做不到 -------------------------------------
type IntFunc func(int) int

func (f IntFunc) Twice(n int) int { return f(f(n)) }

func demoFuncTypeMethod() {
	double := IntFunc(func(n int) int { return n * 2 })
	fmt.Println("6) 函数类型的方法 Twice(5) =", double.Twice(5), "（JS 里得包成对象）")
}

// --- 7. memoize：一模一样的思路 -----------------------------------------
func demoMemoize() {
	sq := memoize(func(n int) int {
		fmt.Println("   计算", n)
		return n * n
	})
	fmt.Println("7) memoize:", sq(4), sq(4), sq(4), sq(5))
}

func memoize(f func(int) int) func(int) int {
	cache := map[int]int{}
	return func(n int) int {
		if v, ok := cache[n]; ok {
			return v
		}
		v := f(n)
		cache[n] = v
		return v
	}
}

// --- 8. 递归闭包：Go 得先声明再赋值 -------------------------------------
//
// JS：const fib = n => n < 2 ? n : fib(n-1) + fib(n-2)   ← 直接可用
// Go：不能写 fib := func(...) 然后在里面引用 fib（此时 fib 还没定义完），
//
//	必须先 var fib func(int) int 声明类型，再赋值。
func demoRecursive() {
	var fib func(int) int
	fib = func(n int) int {
		if n < 2 {
			return n
		}
		return fib(n-1) + fib(n-2)
	}
	fmt.Println("8) 递归闭包 fib(10) =", fib(10))
}

// --- 9. defer：参数立即求值，闭包延迟求值 --------------------------------
//
// JS 没有 defer，最接近的是 try/finally。
func demoDefer() {
	x := 1
	defer fmt.Println("9) defer 参数（立即求值）:", x)              // 1
	defer func() { fmt.Println("9) defer 闭包（执行时读）:", x) }() // 2
	x = 2
}
