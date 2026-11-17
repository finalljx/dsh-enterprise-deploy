# 页面资源

- `templates/`：Go `html/template`。数据由 `pages.go` 的 `loginPageData` / `startingPageData`、
  `admin.go` 的 `adminPageData` 提供。
- `assets/`：静态文件，挂在 `/_dsh/assets/` 下（`base.css`、`starting.js`、`admin.css`、`admin.js`）。

约定：

- 模板只引用 `/_dsh/assets/...`，不要内联 `<style>` / `<script>`：CSP 是 `style-src 'self'; script-src 'self'`。
- 等待页的初始状态与回跳目标走 `<main data-state data-next>`，JS 不经过模板，改前端不用碰 Go。
- `//go:embed` 在编译期打包，改完要重新构建镜像；本地 `go build` 会直接读目录，无需额外步骤。

页面语义（什么时候出现、为什么返 200）见 [docs/login-to-service-flow.md](../../docs/login-to-service-flow.md) 阶段 1.5。
