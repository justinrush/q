import { defineConfig } from "vite";
export default defineConfig({
  server: {
    proxy: {
      "/api": {
        target: process.env.Q_ANALYTICS_URL || "http://127.0.0.1:8765",
        changeOrigin: true,
        configure(proxy) {
          proxy.on("proxyReq", (req) => {
            req.removeHeader("origin");
            req.removeHeader("sec-fetch-site");
          });
        },
      },
    },
  },
});
