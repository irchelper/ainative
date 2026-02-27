<script setup lang="ts">
defineProps<{ show: boolean }>()
defineEmits<{ (e: 'close'): void }>()

const shortcuts = [
  { key: 'j / ↓', desc: '选中下一个任务' },
  { key: 'k / ↑', desc: '选中上一个任务' },
  { key: 'Enter', desc: '打开选中任务详情' },
  { key: 'd', desc: '将选中任务标为 done' },
  { key: 'ESC', desc: '取消选中 / 关闭弹窗' },
  { key: '?', desc: '显示/隐藏此帮助' },
]
</script>

<template>
  <Transition name="fade">
    <div
      v-if="show"
      class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm"
      @click.self="$emit('close')"
    >
      <div class="bg-gray-900 border border-gray-700 rounded-2xl p-6 w-80 shadow-2xl">
        <div class="flex items-center justify-between mb-4">
          <h2 class="text-sm font-semibold text-gray-200">⌨️ 键盘快捷键</h2>
          <button class="text-gray-500 hover:text-white text-lg leading-none" @click="$emit('close')">×</button>
        </div>
        <div class="space-y-2">
          <div
            v-for="s in shortcuts"
            :key="s.key"
            class="flex items-center justify-between text-sm"
          >
            <kbd class="bg-gray-800 border border-gray-600 text-gray-300 text-xs font-mono px-2 py-0.5 rounded">{{ s.key }}</kbd>
            <span class="text-gray-500 text-xs">{{ s.desc }}</span>
          </div>
        </div>
        <p class="text-xs text-gray-600 mt-4">仅在非输入框聚焦时生效</p>
      </div>
    </div>
  </Transition>
</template>

<style scoped>
.fade-enter-active, .fade-leave-active { transition: opacity 0.15s; }
.fade-enter-from, .fade-leave-to { opacity: 0; }
</style>
