import { createRouter, createWebHistory } from 'vue-router'
import DashboardPage from '@/pages/DashboardPage.vue'

const router = createRouter({
  history: createWebHistory('/'),
  routes: [
    { path: '/', name: 'dashboard', component: DashboardPage },
    {
      path: '/goals/new',
      name: 'goal-input',
      component: () => import('@/pages/GoalInputPage.vue'),
    },
    {
      path: '/goals',
      name: 'goal-tracking',
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
    {
      path: '/graph',
      name: 'graph',
      component: () => import('@/pages/GraphVisualizationPage.vue'),
    },
  ],
})

export default router
