import { createRouter, createWebHistory } from 'vue-router'
import DashboardPage from '@/pages/DashboardPage.vue'

const router = createRouter({
  history: createWebHistory('/'),
  routes: [
    {
      path: '/',
      name: 'dashboard',
      component: DashboardPage,
    },
    {
      path: '/goals',
      name: 'goals',
      component: () => import('@/pages/GoalTrackingPage.vue'),
    },
    {
      path: '/kanban',
      name: 'kanban',
      component: () => import('@/pages/KanbanPage.vue'),
    },
    {
      path: '/tasks/:id',
      name: 'task-detail',
      component: () => import('@/pages/TaskDetailPage.vue'),
    },
  ],
})

export default router
