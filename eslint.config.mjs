import eslint from "@eslint/js";

export default [
  {
    ignores: [
      "node_modules/**",
      "coverage/**",
      "packages/db/migrations/**",
      "apps/ui/dist/**",
    ],
  },
  eslint.configs.recommended,
];
