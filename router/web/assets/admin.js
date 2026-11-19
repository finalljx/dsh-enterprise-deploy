// 管理台前端：新增用户、查看列表、休眠/唤醒。
// 纯静态资源；基础域名从 <main id="admin" data-base-domain> 读取。
(function () {
  "use strict";

  var root = document.getElementById("admin");
  var baseDomain = (root && root.dataset.baseDomain) || "dsh.corp";
  var email = document.getElementById("email");
  var host = document.getElementById("host");
  var tier = document.getElementById("tier");
  var token = document.getElementById("model-token");
  var form = document.getElementById("create-form");
  var msg = document.getElementById("form-msg");
  var submit = document.getElementById("submit");
  var rows = document.getElementById("rows");
  var empty = document.getElementById("empty");
  var refresh = document.getElementById("refresh");

  function api(path, opts) {
    opts = opts || {};
    opts.credentials = "same-origin";
    opts.headers = Object.assign({ accept: "application/json" }, opts.headers || {});
    if (opts.body) {
      opts.headers["content-type"] = "application/json";
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 401) {
        location.replace("/_dsh/login?e=expired&next=" + encodeURIComponent("/_dsh/admin"));
        throw new Error("会话已过期，正在重新登录");
      }
      return r.text().then(function (text) {
        var data = {};
        try {
          data = text ? JSON.parse(text) : {};
        } catch (e) {
          data = { error: text };
        }
        if (!r.ok) {
          throw new Error(data.error || "HTTP " + r.status);
        }
        return data;
      });
    });
  }

  function show(text, ok) {
    msg.hidden = false;
    msg.textContent = text;
    msg.className = "msg " + (ok ? "ok" : "err");
  }

  function slugFor(value) {
    var local = (value || "").split("@")[0];
    return local.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "");
  }

  function preview() {
    var slug = slugFor(email.value);
    host.value = slug ? slug + "." + baseDomain : "";
  }

  function fmtTime(ts) {
    if (!ts) {
      return "—";
    }
    var diff = Math.floor(Date.now() / 1000) - ts;
    if (diff < 60) {
      return "刚活动";
    }
    if (diff < 3600) {
      return Math.floor(diff / 60) + " 分钟前";
    }
    if (diff < 86400) {
      return Math.floor(diff / 3600) + " 小时前";
    }
    return Math.floor(diff / 86400) + " 天前";
  }

  function statusOf(user) {
    if (user.podIP) {
      return { cls: "running", text: "运行中" };
    }
    if (user.desiredState === "archived") {
      return { cls: "archived", text: "已归档" };
    }
    if (user.desiredState === "sleeping") {
      return { cls: "sleeping", text: "休眠" };
    }
    return { cls: "starting", text: "启动中" };
  }

  function cell(text, cls) {
    var td = document.createElement("td");
    if (cls) {
      td.className = cls;
    }
    td.textContent = text;
    return td;
  }

  function setDesired(subject, desiredState) {
    return api("/_dsh/admin/api/desired", {
      method: "POST",
      body: JSON.stringify({
        subject: subject,
        desiredState: desiredState,
        clearPod: desiredState === "sleeping"
      })
    }).then(function () {
      show(desiredState === "running" ? "已唤醒 " + subject : "已休眠 " + subject, true);
      return load();
    }).catch(function (err) {
      show(err.message, false);
    });
  }

  function actionButton(user) {
    var sleeping = user.desiredState === "sleeping";
    var button = document.createElement("button");
    button.type = "button";
    button.className = "ghost small";
    button.textContent = sleeping ? "唤醒" : "休眠";
    button.addEventListener("click", function () {
      button.disabled = true;
      setDesired(user.subject, sleeping ? "running" : "sleeping").then(function () {
        button.disabled = false;
      });
    });
    return button;
  }

  function deleteButton(user) {
    var button = document.createElement("button");
    button.type = "button";
    button.className = "ghost small danger";
    button.textContent = "删除";
    button.addEventListener("click", function () {
      var msg = "删除 " + user.subject + "？\n\n" +
        "会停止并删除它的工作负载与凭据，用户立即无法访问。\n" +
        "数据库记录保留为软删除状态（数据卷也保留），可随时重新开通恢复。\n" +
        "如需彻底清除数据，请在命令行加 keepData=0 重试。";
      if (!window.confirm(msg)) return;
      button.disabled = true;
      show("正在删除 " + user.subject + "…", true);
      api("/_dsh/admin/api/users?subject=" + encodeURIComponent(user.subject), { method: "DELETE" })
        .then(function () {
          show("已删除 " + user.subject + "（软删除，数据保留）", true);
          return load();
        })
        .catch(function (err) {
          show(err.message, false);
          button.disabled = false;
        });
    });
    return button;
  }

  function render(users) {
    rows.textContent = "";
    empty.hidden = users.length > 0;
    users.forEach(function (user) {
      var tr = document.createElement("tr");
      var status = statusOf(user);
      var badge = document.createElement("span");
      badge.className = "badge " + status.cls;
      badge.textContent = status.text;

      var statusCell = document.createElement("td");
      statusCell.appendChild(badge);

      var actionCell = document.createElement("td");
      if (user.desiredState !== "archived") {
        actionCell.appendChild(actionButton(user));
      }
      actionCell.appendChild(deleteButton(user));

      tr.appendChild(cell(user.subject, "subject"));
      tr.appendChild(cell(user.publicHost, "mono"));
      tr.appendChild(statusCell);
      tr.appendChild(cell(user.podIP || "—", "mono"));
      tr.appendChild(cell(fmtTime(user.lastActivityAt), "mono"));
      tr.appendChild(actionCell);
      rows.appendChild(tr);
    });
  }

  function load() {
    return api("/_dsh/admin/api/users").then(function (list) {
      render(Array.isArray(list) ? list : []);
    }).catch(function (err) {
      show(err.message, false);
    });
  }

  email.addEventListener("input", preview);

  form.addEventListener("submit", function (event) {
    event.preventDefault();
    var subject = email.value.trim().toLowerCase();
    if (subject.indexOf("@") < 0) {
      show("请填写完整的企业邮箱", false);
      return;
    }
    if (!token.value.trim()) {
      show("请填写模型密钥", false);
      return;
    }
    submit.disabled = true;
    show("正在创建 " + subject + "，大约需要 10–20 秒…", true);
    api("/_dsh/admin/api/users", {
      method: "POST",
      body: JSON.stringify({
        subject: subject,
        tier: tier.value,
        modelToken: token.value.trim()
      })
    }).then(function (res) {
      show("已创建 " + res.subject + "（" + res.host + "），由回收器拉起到运行中。", true);
      token.value = "";
      return load();
    }).catch(function (err) {
      show(err.message, false);
    }).then(function () {
      submit.disabled = false;
    });
  });

  refresh.addEventListener("click", function () {
    refresh.disabled = true;
    load().then(function () {
      refresh.disabled = false;
    });
  });

  window.setInterval(function () {
    if (!document.hidden) {
      load();
    }
  }, 10000);

  preview();
  load();
})();
