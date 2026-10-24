<script setup lang="ts">
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 18 })
</script>

<template>
  <svg
    class="app-icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.7"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <template v-if="name === 'overview'">
      <rect x="3.5" y="3.5" width="7" height="7" rx="1.6" />
      <rect x="13.5" y="3.5" width="7" height="7" rx="1.6" />
      <rect x="3.5" y="13.5" width="7" height="7" rx="1.6" />
      <rect x="13.5" y="13.5" width="7" height="7" rx="1.6" />
    </template>
    <template v-else-if="name === 'plugins'">
      <path d="M12 3.2 20 7.5v9L12 20.8 4 16.5v-9L12 3.2z" />
      <path d="M4 7.5l8 4.3 8-4.3" />
      <path d="M12 11.8v9" />
    </template>
    <template v-else-if="name === 'accounts'">
      <circle cx="9" cy="8" r="3.2" />
      <path d="M3.6 20c0-3 2.4-5.2 5.4-5.2s5.4 2.2 5.4 5.2" />
      <path d="M16.2 5.6a2.8 2.8 0 0 1 0 5.4" />
      <path d="M17.8 14.8c1.6.7 2.6 2.1 2.6 3.9" />
    </template>
    <template v-else-if="name === 'checkin'">
      <rect x="3.5" y="5" width="17" height="15.5" rx="2" />
      <path d="M8 3v4M16 3v4M3.5 9.5h17" />
      <path d="m8.5 14.4 2.2 2.2 4.3-4.3" />
    </template>
    <template v-else-if="name === 'usage'">
      <path d="M5 20v-7M10 20V5M15 20v-9M20 20V4" />
    </template>
    <template v-else-if="name === 'scheduler'">
      <circle cx="6" cy="6" r="2.5" />
      <circle cx="18" cy="18" r="2.5" />
      <path d="M6 8.5v3A3.5 3.5 0 0 0 9.5 15h5a3 3 0 0 1 3 3" />
    </template>
    <template v-else-if="name === 'pools'">
      <path d="M12 3.5 20.5 8 12 12.5 3.5 8 12 3.5z" />
      <path d="M3.5 12.5 12 17l8.5-4.5" />
      <path d="M3.5 16.5 12 21l8.5-4.5" />
    </template>
    <template v-else-if="name === 'rules'">
      <path d="M4 5h16l-6.2 7.2V19l-3.6 2v-8.8L4 5z" />
    </template>
    <template v-else-if="name === 'proxies'">
      <circle cx="12" cy="12" r="8.5" />
      <ellipse cx="12" cy="12" rx="3.6" ry="8.5" />
      <path d="M3.5 12h17" />
    </template>
    <template v-else-if="name === 'logs'">
      <path d="M8 6.5h13M8 12h13M8 17.5h13" />
      <circle cx="4" cy="6.5" r="1" fill="currentColor" stroke="none" />
      <circle cx="4" cy="12" r="1" fill="currentColor" stroke="none" />
      <circle cx="4" cy="17.5" r="1" fill="currentColor" stroke="none" />
    </template>
    <template v-else-if="name === 'settings'">
      <path d="M4 7h9M17 7h3M4 17h4M12 17h8" />
      <circle cx="15" cy="7" r="2" />
      <circle cx="10" cy="17" r="2" />
    </template>
    <template v-else-if="name === 'sun'">
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2.5v2M12 19.5v2M2.5 12h2M19.5 12h2M5.3 5.3l1.4 1.4M17.3 17.3l1.4 1.4M18.7 5.3l-1.4 1.4M6.7 17.3l-1.4 1.4" />
    </template>
    <template v-else-if="name === 'moon'">
      <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z" />
    </template>
    <template v-else-if="name === 'zap'">
      <path d="M13 2.5 4.5 13.5H11l-1 8L19.5 10H13l0-7.5z" />
    </template>
  </svg>
</template>

<style scoped>
.app-icon {
  flex: none;
  display: block;
}
</style>
