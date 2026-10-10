import react from '@vitejs/plugin-react'
import jotaiDebugLabel from 'jotai/babel/plugin-debug-label'
import jotaiReactRefresh from 'jotai/babel/plugin-react-refresh'
import { defineConfig, splitVendorChunkPlugin } from 'vite'
import { VitePWA } from 'vite-plugin-pwa'
import tsConfigPath from 'vite-tsconfig-paths'

export default defineConfig(
    env => ({
        plugins: [
            // only use react-fresh
            env.mode === 'development' && react({
                babel: { plugins: [jotaiDebugLabel, jotaiReactRefresh] },
            }),
            tsConfigPath(),
            VitePWA({
                injectRegister: 'inline',
                registerType: 'autoUpdate',
                manifest: {
                    icons: [{
                        src: './Icon.png',
                        sizes: '512x512',
                        type: 'image/png',
                    }],
                    start_url: './',
                    short_name: 'Clash Dashboard',
                    name: 'Clash Dashboard',
                    theme_color: '#141e2f',
                    background_color: '#f3f6f9',
                },
            }),
            splitVendorChunkPlugin(),
        ],
        server: {
            port: 3000,
        },
        base: './',
        build: {
            reportCompressedSize: false,
            rollupOptions: {
                onwarn (warning, warn) {
                    // motion ships "use client" markers for server components; they are meaningless in this SPA.
                    if (warning.code === 'MODULE_LEVEL_DIRECTIVE' && warning.message.includes('use client')) return
                    warn(warning)
                },
            },
        },
        esbuild: {
            jsxInject: "import React from 'react'",
        },
    }),
)
