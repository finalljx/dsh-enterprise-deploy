// Package main 演示 Go 并发（goroutine）的标准用法。
//
// 运行：go run ./examples/goroutine
// 竞态检测：go run -race ./examples/goroutine
//
// 每个 demo 对应一个真实的工程场景，输出里带耗时，方便观察并行效果。
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func main() {
	step := func(name string, fn func()) {
		fmt.Printf("\n===== %s =====\n", name)
		start := time.Now()
		fn()
		fmt.Printf("(耗时 %v)\n", time.Since(start).Round(time.Millisecond))
	}

	step("1. 并行调用多个下游服务", demoFanOut)
	step("2. 用 channel 收集结果（不共享内存）", demoChannelCollect)
	step("3. Worker Pool：固定并发度处理任务", demoWorkerPool)
	step("4. 超时与取消（context）", demoTimeout)
	step("5. Pipeline：流式处理", demoPipeline)
	step("6. select 取最快返回（Promise.race）", demoRace)
	step("7. 后台定时任务（ticker）", demoTicker)
	step("8. sync.Once 并发安全初始化", demoOnce)
	step("9. 任一失败立即取消其余任务", demoFirstError)
	test()
}

// ---------------------------------------------------------------------------
// 1. 并行调用多个下游服务（最常见的场景）
//
// 场景：一个页面要聚合用户、订单、支付三个服务的数据。
// 串行 = 300+300+300 = 900ms；并行 = 300ms。
// ---------------------------------------------------------------------------
func demoFanOut() {
	services := []string{"user-svc", "order-svc", "pay-svc"}

	// --- 串行对照 ---
	start := time.Now()
	serial := make([]string, len(services))
	for i, svc := range services {
		time.Sleep(300 * time.Millisecond) // 模拟 RPC
		serial[i] = svc + ": ok"
	}
	fmt.Printf("串行: %v  %v\n", time.Since(start).Round(10*time.Millisecond), serial)

	// --- 并行 ---
	start = time.Now()
	results := make([]string, len(services)) // 每个 goroutine 写不同的下标，无需加锁
	var wg sync.WaitGroup

	for i, svc := range services {
		wg.Add(1) // 必须在 go 之前调用，否则可能在 Wait 之后才 Add
		go func(i int, svc string) {
			defer wg.Done() // 等价于 finally：保证一定执行，panic 也执行
			time.Sleep(300 * time.Millisecond)
			results[i] = svc + ": ok"
		}(i, svc) // 显式传参最清晰（Go 1.22+ 已支持隐式捕获每轮变量）
	}

	wg.Wait() // 阻塞到计数归零
	fmt.Printf("并行: %v  %v\n", time.Since(start).Round(10*time.Millisecond), results)
}

// ---------------------------------------------------------------------------
// 2. 用 channel 收集结果
//
// "不要通过共享内存来通信，而要通过通信来共享内存。"
// 带缓冲的 channel 让发送方不用等接收方就绪，天然适合"收集 N 个结果"。
// ---------------------------------------------------------------------------
func demoChannelCollect() {
	type result struct {
		id  int
		val int
	}

	const n = 3
	ch := make(chan result, n) // 缓冲大小 = 结果数量，避免发送方阻塞

	for i := 1; i <= n; i++ {
		go func(id int) {
			// 故意让完成顺序和启动顺序不同
			time.Sleep(time.Duration(n-id+1) * 50 * time.Millisecond)
			ch <- result{id: id, val: id * id}
		}(i)
	}

	// 按完成顺序接收，而不是启动顺序
	for i := 0; i < n; i++ {
		r := <-ch
		fmt.Printf("收到 id=%d val=%d\n", r.id, r.val)
	}
}

// ---------------------------------------------------------------------------
// 3. Worker Pool：固定并发度
//
// 场景：10000 条消息要处理，但下游只能承受 3 个并发。
// 起 10000 个 goroutine 会打爆下游；起 3 个 worker 消费队列才是正解。
// ---------------------------------------------------------------------------
func demoWorkerPool() {
	const (
		workers = 3
		jobNum  = 9
	)

	jobs := make(chan int, jobNum)
	results := make(chan int, jobNum)

	var wg sync.WaitGroup
	for w := 1; w <= workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			// range 会一直阻塞等待，直到 jobs 被 close
			for j := range jobs {
				time.Sleep(100 * time.Millisecond) // 模拟耗时处理
				results <- j * 2
			}
		}(w)
	}

	// 生产者：投递完必须 close，否则 worker 的 range 永远不结束
	go func() {
		for j := 1; j <= jobNum; j++ {
			jobs <- j
		}
		close(jobs)
	}()

	// 等所有 worker 退出后再关 results，接收方才能安全地 range
	go func() {
		wg.Wait()
		close(results)
	}()

	sum := 0
	for r := range results {
		sum += r
	}
	fmt.Printf("%d 个任务 / %d 个 worker，结果和 = %d\n", jobNum, workers, sum)
}

// ---------------------------------------------------------------------------
// 4. 超时与取消（context）
//
// 场景：调用一个可能很慢的下游，必须 200ms 内放弃。
// 关键点：超时后 goroutine 必须能从 ctx.Done() 退出，否则就是泄漏。
// ---------------------------------------------------------------------------
func demoTimeout() {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel() // 一定要调用，释放 ctx 关联的定时器（否则内存泄漏 + go vet 警告）

	ch := make(chan string, 1) // 缓冲 1：即使没人接收也不会泄漏发送方
	go func() {
		select {
		case <-time.After(500 * time.Millisecond): // 模拟慢调用
			ch <- "慢结果"
		case <-ctx.Done(): // 被取消就立刻退出
			fmt.Println("  worker 收到取消信号，提前退出")
		}
	}()

	select {
	case v := <-ch:
		fmt.Println("拿到:", v)
	case <-ctx.Done():
		fmt.Println("  上层超时:", ctx.Err())
	}

	time.Sleep(400 * time.Millisecond) // 留时间让上面那行"提前退出"打印出来
}

// ---------------------------------------------------------------------------
// 5. Pipeline：流式处理
//
// 场景：读大文件 → 解析 → 转换 → 写库。每级一个 goroutine，用 channel 串联，
// 内存占用是"流水线"而不是"全量加载"。
// <-chan int 表示只读 channel，由编译器保证不会误写。
// ---------------------------------------------------------------------------
func demoPipeline() {
	gen := func(nums ...int) <-chan int {
		out := make(chan int)
		go func() {
			defer close(out) // 关闭是"没有更多数据"的唯一信号
			for _, n := range nums {
				out <- n
			}
		}()
		return out
	}

	square := func(in <-chan int) <-chan int {
		out := make(chan int)
		go func() {
			defer close(out)
			for n := range in {
				out <- n * n
			}
		}()
		return out
	}

	// gen -> square -> 消费方，三级流水线
	var got []int
	for v := range square(gen(1, 2, 3, 4, 5)) {
		got = append(got, v)
	}
	fmt.Println("原始 [1 2 3 4 5] -> 平方", got)
}

// ---------------------------------------------------------------------------
// 6. select：先到先得
//
// 场景：同时查缓存和数据库（或主备两个接口），谁先返回用谁。
// 相当于 JS 的 Promise.race。
// ---------------------------------------------------------------------------
func demoRace() {
	// 都带缓冲 1：输掉的那一方发送时不会永远阻塞
	cache := make(chan string, 1)
	db := make(chan string, 1)

	go func() {
		time.Sleep(80 * time.Millisecond)
		cache <- "来自缓存"
	}()
	go func() {
		time.Sleep(150 * time.Millisecond)
		db <- "来自数据库"
	}()

	select {
	case v := <-cache:
		fmt.Println("先到:", v)
	case v := <-db:
		fmt.Println("先到", v)
	case <-time.After(500 * time.Millisecond): // 兜底超时，防止永久阻塞
		fmt.Println("全部超时")
	}
}

// ---------------------------------------------------------------------------
// 7. 后台定时任务
//
// 场景：每 N 秒清理过期缓存、上报指标、刷新配置。
// time.Tick 会泄漏（无法停止），生产代码用 NewTicker + defer Stop()。
// ---------------------------------------------------------------------------
func demoTicker() {
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done(): // 收到退出信号
				fmt.Println("  定时任务已停止")
				return
			case <-ticker.C:
				fmt.Println("  执行清理任务")
			}
		}
	}()

	time.Sleep(350 * time.Millisecond)
	cancel()                           // 通知退出
	time.Sleep(100 * time.Millisecond) // 等它打印"已停止"
}

// ---------------------------------------------------------------------------
// 8. sync.Once：并发安全的初始化
//
// 场景：懒加载数据库连接、读配置、注册指标。多个请求同时到达，
// 只允许初始化一次，其余人阻塞等待同一个结果。
// ---------------------------------------------------------------------------
func demoOnce() {
	var (
		once sync.Once
		conn string
	)

	initConn := func() {
		fmt.Println("  初始化连接（只会打印一次）")
		time.Sleep(100 * time.Millisecond)
		conn = "db://connected"
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			once.Do(initConn) // 并发安全：只有一个 goroutine 真正执行
			_ = conn          // Do 返回后读 conn 是安全的（happens-before）
		}(i)
	}
	wg.Wait()
	fmt.Println("  conn =", conn)
}

// ---------------------------------------------------------------------------
// 9. 任一失败立即取消其余任务
//
// 场景：一个请求依赖 3 个下游，任意一个失败就没必要继续了，立刻取消省资源。
// 这就是 golang.org/x/sync/errgroup 做的事，这里用标准库手写等价物。
// ---------------------------------------------------------------------------
func demoFirstError() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type task struct {
		name string
		cost time.Duration
		fail bool
	}
	tasks := []task{
		{name: "cache", cost: 120 * time.Millisecond},
		{name: "db", cost: 80 * time.Millisecond, fail: true},
		{name: "search", cost: 200 * time.Millisecond},
	}

	var (
		wg       sync.WaitGroup
		once     sync.Once
		firstErr error
	)

	for _, t := range tasks {
		wg.Add(1)
		go func(t task) {
			defer wg.Done()
			select {
			case <-time.After(t.cost):
				if !t.fail {
					fmt.Printf("  %s 成功\n", t.name)
					return
				}
				// 只记录第一个错误，并取消其他任务
				once.Do(func() {
					firstErr = fmt.Errorf("%s 失败，取消其余任务", t.name)
					cancel()
				})
			case <-ctx.Done():
				fmt.Printf("  %s 被取消\n", t.name)
			}
		}(t)
	}

	wg.Wait()
	fmt.Println("  firstErr:", firstErr)
}

type Counter struct {
	N int
}

func (c *Counter) Inc() { c.N++ }
func newCounter() (inc func(), get func() int) {
	n := 0
	inc = func() { n++ }
	get = func() int { return n }
	return
}

func test() {
	inc, get := newCounter()
	inc()
	inc()
	fmt.Println("test get=", get())
}
