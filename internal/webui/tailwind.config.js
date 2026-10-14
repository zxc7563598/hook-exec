/** Tailwind 配置。只在 `make css` 构建静态样式时用到，不参与运行时。
 *
 *  设计令牌基本都落在 Tailwind 默认色阶上，所以这里只需要补一个：
 *  panel 是比卡片再高一层的浮层底色（弹窗、抽屉、代码块）。
 */
module.exports = {
  // 类名全部写在 index.html 里（含 x-template 中的内联模板和页面脚本），
  // Tailwind 是把它当纯文本扫描的，所以这一个文件就够了。
  content: ["./index.html"],
  theme: {
    extend: {
      colors: {
        panel: "#272F42",
      },
      fontFamily: {
        // 系统字体栈：不引 Google Fonts，内网部署时首屏不会卡在外链上。
        // 同时显式列出中文字体，避免中文落到某个衬线兜底字体上。
        sans: [
          "system-ui", "-apple-system", "BlinkMacSystemFont", "Segoe UI",
          "Roboto", "Helvetica Neue", "PingFang SC", "Hiragino Sans GB",
          "Microsoft YaHei", "sans-serif",
        ],
        mono: [
          "ui-monospace", "SFMono-Regular", "SF Mono", "Menlo", "Consolas",
          "Liberation Mono", "monospace",
        ],
      },
    },
  },
  plugins: [],
};
