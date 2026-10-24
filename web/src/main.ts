import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import { exchangeLaunchToken } from './api'
import { installRipple } from './ripple'
import './styles.css'

// The host opens this page as http://127.0.0.1:<port>/?t=<token>. That one-shot
// token is the only way to obtain the admin session cookie: every /api request,
// reads included, is rejected without it. So the exchange must finish before the
// app mounts and fires its first call (App.vue's status probe runs on mount).
//
// exchangeLaunchToken (api.ts) owns the whole handoff: it posts the token to
// /api/session and strips ?t= from the address bar, so the token does not linger
// in history, screenshots or copy-pasted links. It is a no-op for a page opened
// without a token (a bookmark, a second tab, a reload after the token was
// spent): the shell still renders, the API answers 403 admin_session_required,
// and nothing is silently granted.
await exchangeLaunchToken()

installRipple()

createApp(App).use(router).mount('#app')
