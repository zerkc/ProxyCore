import eslint from "@eslint/js";

export default [
  {
    ignores: ["node_modules/**", "coverage/**", "apps/ui/dist/**"],
  },
  eslint.configs.recommended,
];
