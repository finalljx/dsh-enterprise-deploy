// Go 闭包示例的 JavaScript 对照版。
// 运行：node demo.mjs
// 与 main.go 的编号一一对应，方便逐条比对输出。

// 1. 计数器
function newCounter() {
  let n = 0;
  return { inc: () => n++, get: () => n };
}

// 2. 捕获的是「变量」
function demoCaptureVariable() {
  let x = 1;
  const f = () => x;
  x = 100;
  console.log('2) 捕获变量: f() =', f(), '（外部改成 100 了）');
}

// 3. 显式捕获值
const captureValue = (v) => () => v;

function demoCaptureValue() {
  let y = 7;
  const g = captureValue(y);
  y = 999;
  console.log('3) 显式捕获值: g() =', g(), '（外部 y 变了也不受影响）');
}

// 4. 循环变量：var 共享 vs let 每轮独立
//    Go 1.21 及以前 = var 行为；Go 1.22+ = let 行为。
function demoLoopVar() {
  const withVar = [];
  for (var i = 0; i < 3; i++) withVar.push(() => i);
  console.log('4a) var 循环:', withVar.map((f) => f()));

  const withLet = [];
  for (let j = 0; j < 3; j++) withLet.push(() => j);
  console.log('4b) let 循环:', withLet.map((f) => f()));
}

// 5. 异步回调 + 循环变量（对应 Go 的 goroutine 版本，但无需加锁）
function demoAsyncLoop() {
  return new Promise((resolve) => {
    const got = [];
    for (let k = 0; k < 3; k++) {
      setTimeout(() => {
        got.push(k); // 单线程，不需要 mutex
        if (got.length === 3) {
          console.log('5) setTimeout 拿到的 k:', got, '（顺序不定）');
          resolve();
        }
      }, 10);
    }
  });
}

// 6. JS 里函数是对象，但你没法给「函数类型」加方法；
//    只能包一层对象，调用形态从 double(5) 变成 obj.twice(5)。
class IntFunc {
  constructor(fn) { this.fn = fn; }
  twice(n) { return this.fn(this.fn(n)); }
}
function demoFuncTypeMethod() {
  const double = new IntFunc((n) => n * 2);
  console.log('6) 包成对象后才能加方法 twice(5) =', double.twice(5));
}

// 7. memoize
function memoize(fn) {
  const cache = new Map();
  return (n) => {
    if (cache.has(n)) return cache.get(n);
    const v = fn(n);
    cache.set(n, v);
    return v;
  };
}

function demoMemoize() {
  const sq = memoize((n) => {
    console.log('   计算', n);
    return n * n;
  });
  console.log('7) memoize:', sq(4), sq(4), sq(4), sq(5));
}

// 8. 递归闭包：const 直接可用（函数体执行时 fib 已绑定）
function demoRecursive() {
  const fib = (n) => (n < 2 ? n : fib(n - 1) + fib(n - 2));
  console.log('8) 递归闭包 fib(10) =', fib(10));
}

// 9. 没有 defer，只有 try/finally
function demoDefer() {
  let x = 1;
  try {
    x = 2;
  } finally {
    console.log('9) finally 里读 x（执行时求值）:', x);
  }
}

// ---------------- 运行 ----------------
const c = newCounter();
c.inc(); c.inc(); c.inc();
console.log('1) 计数器:', c.get());

demoCaptureVariable();
demoCaptureValue();
demoLoopVar();
demoFuncTypeMethod();
demoMemoize();
demoRecursive();
demoDefer();
await demoAsyncLoop();
