import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'dashboard',
    component: () => import('./views/Dashboard.vue'),
    meta: { title: '概览', subtitle: '宿主与插件运行状态' },
  },
  {
    path: '/plugins',
    name: 'plugins',
    component: () => import('./views/Plugins.vue'),
    meta: { title: '插件', subtitle: '安装、启停与卸载独立插件' },
  },
  {
    path: '/accounts',
    name: 'accounts',
    component: () => import('./views/Accounts.vue'),
    meta: { title: '账号', subtitle: '额度、设备身份与出口代理' },
  },
  {
    path: '/checkin',
    name: 'checkin',
    component: () => import('./views/Checkin.vue'),
    meta: { title: '签到', subtitle: '账号 × 日期的签到记录' },
  },
  {
    path: '/usage',
    name: 'usage',
    component: () => import('./views/Usage.vue'),
    meta: { title: '用量', subtitle: '请求、积分与账号到期情况' },
  },
  {
    path: '/scheduler',
    name: 'scheduler',
    component: () => import('./views/Scheduler.vue'),
    meta: { title: '调度策略中心', subtitle: '池间/池内策略、模型绑定与调度解释' },
  },
  {
    path: '/pools',
    name: 'pools',
    component: () => import('./views/Pool.vue'),
    meta: { title: '资源', subtitle: '按插件查看积分、账号池与模型目录' },
  },
  {
    path: '/pools/:id',
    name: 'pool',
    component: () => import('./views/Pool.vue'),
    meta: { title: '资源调度', subtitle: '单插件的调度参数与资源视图' },
  },
  {
    path: '/rules',
    name: 'rules',
    component: () => import('./views/Rules.vue'),
    meta: { title: '指纹清洗', subtitle: '共享规则包：宿主下发，插件执行' },
  },
  {
    path: '/proxies',
    name: 'proxies',
    component: () => import('./views/Proxies.vue'),
    meta: { title: '代理', subtitle: '账号级出网代理池' },
  },
  {
    path: '/logs',
    name: 'logs',
    component: () => import('./views/Logs.vue'),
    meta: { title: '日志', subtitle: '统一事件时间线与请求正文' },
  },
  {
    path: '/settings',
    name: 'settings',
    component: () => import('./views/Settings.vue'),
    meta: { title: '设置', subtitle: '宿主端口、A 接入与保留策略' },
  },
  {
    path: '/:pathMatch(.*)*',
    redirect: '/',
  },
]

export const router = createRouter({
  history: createWebHistory('/'),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})
