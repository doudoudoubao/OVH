// ESLint 扁平配置（ESLint 9）。
//
// 为什么现在才有这个文件:仓库里一直有 `"lint": "eslint ."` 和 7 处
// `// eslint-disable-next-line react-hooks/exhaustive-deps`,也就是说代码本来
// 就是照着「有 ESLint」写的 —— 只是配置文件从来没进过版本库,于是 `npm run lint`
// 一跑就报 "couldn't find an eslint.config.js"。补回来。
//
// 取舍:这套规则的目标是**在当前这棵树上跑绿**,不是把风格拉齐。
// 一个上来就红 197 条的检查等于没有检查 —— 谁都会学会无视它。
// 所以只留「命中基本就是写错了」的规则当 error,风格类的一律关掉或降成 warn,
// 理由逐条写在下面。
import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

export default tseslint.config(
  {
    ignores: [
      "dist",
      ".tanstack",
      // @tanstack/router-plugin 生成的,不在版本库里,自带 /* eslint-disable */
      "src/routeTree.gen.ts",
      // shadcn-ui 抄进来的第三方组件,不是我们维护的代码
      "src/components/ui/**",
    ],
  },
  {
    files: ["**/*.{ts,tsx}"],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,

      // --- error:命中基本就是写错了 ---

      // rules-of-hooks 是崩溃级的:hook 调用顺序一变 React 内部状态就错位。
      // 当前 0 命中,留着挡以后。
      "react-hooks/rules-of-hooks": "error",

      // 声明了没用 = 要么是删了一半的死代码,要么是写错了变量名。
      // 当前 0 命中(已清理)。下划线开头的形参是「我知道它没用」的惯例,放行。
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],

      // --- warn:是问题但改动有风险,不拦 CI ---

      // exhaustive-deps 抓的是闭包过期(页面显示旧数据)。当前有 9 处命中,
      // 全在上游写的组件里,补依赖会改变 re-render 时机 —— 盲改的风险大于收益。
      // 留成 warn:看得见,但不拦 CI。
      "react-hooks/exhaustive-deps": "warn",
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],

      // --- off:纯风格,而且是别人代码里的风格 ---

      // no-explicit-any 当前 166 处命中。这是整个前端的既定写法,
      // 不是 bug;要开就得改 166 个地方的别人的代码,换来的只是风格统一,
      // 代价是以后每次合上游都冲突。不开。
      "@typescript-eslint/no-explicit-any": "off",

      // `timer && clearTimeout(timer)` / `s.has(x) ? s.delete(x) : s.add(x)`
      // 这类短路当语句用,本项目里是有意为之,读着也清楚。
      "@typescript-eslint/no-unused-expressions": "off",
    },
  },
  {
    // 构建期配置文件跑在 Node 里,而且 tailwind 插件本来就得用 require()
    files: ["*.config.{js,ts}", "vite.config.ts", "postcss.config.js"],
    languageOptions: { globals: globals.node },
    rules: {
      "@typescript-eslint/no-require-imports": "off",
    },
  },
);
