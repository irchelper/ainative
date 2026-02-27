import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '@/api/client'
import type { ConfigData } from '@/types'

export const useConfigStore = defineStore('config', () => {
  const config = ref<ConfigData | null>(null)

  async function load() {
    try {
      config.value = await api.getConfig()
    } catch {
      // fallback: empty config
      config.value = { agents: [], version: 'unknown' }
    }
  }

  return { config, load }
})
