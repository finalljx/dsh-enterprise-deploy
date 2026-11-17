// 启动等待页的前端逻辑。纯静态文件：不经过 Go 模板，
// 状态与回跳目标从 <main id="starting" data-state data-next> 读取。
(function () {
  "use strict";
  var root = document.getElementById("starting");
  var NEXT = (root && root.dataset.next) || "/";
  var state = (root && root.dataset.state) || "starting";
  var COPY = {
    starting: ["正在为你启动工作区", "首次打开或刚唤醒时需要几秒到十几秒，完成后会自动进入。"],
    not_provisioned: ["工作区尚未开通", "你的账号还没有工作区，请联系 IT 管理员开通。页面会自动重试。"],
    archived: ["工作区已归档", "长期未使用已归档，请联系 IT 管理员恢复。"],
    error: ["控制面暂时不可用", "正在自动重试，通常很快恢复。"],
    forbidden: ["无权访问", "这个地址不属于当前账号，请回到自己的工作区。"],
    expired: ["会话已过期", "正在重新登录…"]
  };
  var NOTES = {
    starting: "完成后会自动跳转，无需刷新",
    not_provisioned: "等待开通，页面会自动重试",
    archived: "如需恢复，请联系 IT 管理员",
    error: "正在自动重试…",
    forbidden: "请回到自己的工作区"
  };
  var titleEl = document.querySelector("h1");
  var detailEl = document.querySelector(".detail");
  var stepsEl = document.querySelector(".steps");
  var meterEl = document.querySelector(".meter");
  var noteEl = document.getElementById("note");
  var elapsedEl = document.getElementById("elapsed");
  var retryEl = document.getElementById("retry");
  var workEl = document.getElementById("step-work");
  var enterEl = document.getElementById("step-enter");
  var startedAt = Date.now();
  var stopped = false;
  var current = state;

  function setState(s) {
    var c = COPY[s] || COPY.error;
    titleEl.textContent = c[0];
    detailEl.textContent = c[1];
    var running = s === "starting";
    if (stepsEl) { stepsEl.hidden = !running; }
    if (meterEl) { meterEl.hidden = !running; }
    // 启动超过 90 秒会显示「启动时间较长」，不要把那条提示盖回去。
    if (!(running && !retryEl.hidden)) {
      noteEl.textContent = NOTES[s] || NOTES.error;
    }
  }

  function goLogin() {
    stopped = true;
    location.replace("/_dsh/login?e=expired&next=" +
      encodeURIComponent(location.pathname + location.search));
  }

  function ready() {
    stopped = true;
    workEl.className = "done";
    enterEl.className = "active";
    noteEl.textContent = "已就绪，正在进入…";
    window.setTimeout(function () { location.replace(NEXT || "/"); }, 250);
  }

  function apply(d) {
    if (!d || !d.state) { return; }
    current = d.state;
    if (d.state === "ready") { ready(); return; }
    if (d.state === "expired") { goLogin(); return; }
    setState(d.state);
    if (d.state === "archived" || d.state === "forbidden") { stopped = true; }
  }

  function schedule(ms) {
    if (!stopped) { window.setTimeout(tick, ms); }
  }

  function tick() {
    if (stopped) { return; }
    elapsedEl.textContent = String(Math.floor((Date.now() - startedAt) / 1000));
    if (Date.now() - startedAt > 90000 && retryEl.hidden) {
      retryEl.hidden = false;
      noteEl.textContent = "启动时间较长，可点击重试";
    }
    var ctl = (typeof AbortController !== "undefined") ? new AbortController() : null;
    var timer = window.setTimeout(function () { if (ctl) { ctl.abort(); } }, 4000);
    fetch("/_dsh/status", {
      headers: { "accept": "application/json" },
      cache: "no-store",
      credentials: "same-origin",
      signal: ctl ? ctl.signal : undefined
    }).then(function (r) {
      if (r.status === 401) { goLogin(); return null; }
      if (!r.ok) { throw new Error("http " + r.status); }
      return r.json();
    }).then(function (d) {
      window.clearTimeout(timer);
      apply(d);
      schedule(d && d.state === "not_provisioned" ? 8000 : 2000);
    }).catch(function () {
      window.clearTimeout(timer);
      if (stopped) { return; }
      // 一次网络抖动不该把「正在启动」变成错误页，只有状态接口明确报错才切。
      if (current === "starting" || current === "not_provisioned") {
        noteEl.textContent = "连接中断，正在重试…";
      } else {
        setState("error");
      }
      schedule(4000);
    });
  }

  retryEl.addEventListener("click", function () {
    location.replace(location.pathname + location.search);
  });
  window.setInterval(function () {
    if (!stopped) { elapsedEl.textContent = String(Math.floor((Date.now() - startedAt) / 1000)); }
  }, 1000);
  setState(state);
  tick();
})();
