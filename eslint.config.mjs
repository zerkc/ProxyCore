import eslint from "@eslint/js";
import next from "@next/eslint-plugin-next";

export default [
  {
    ignores: [".next/**", "node_modules/**", "coverage/**", "packages/db/migrations/**"],
  },
  eslint.configs.recommended,
  {
    plugins: {
      "@next/next": next,
    },
    rules: {
      "@next/next/no-html-link-for-pages": "off",
    },
  },
];
