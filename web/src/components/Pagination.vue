<script setup lang="ts">
const props = defineProps<{
  page: number
  totalPages: number
  total: number
}>()

const emit = defineEmits<{
  (e: 'go', n: number): void
}>()

// Show at most 7 page buttons: 1 … p-1 p p+1 … last
function pages(): (number | '…')[] {
  const { page, totalPages } = props
  if (totalPages <= 7) return Array.from({ length: totalPages }, (_, i) => i + 1)
  const set = new Set([1, totalPages, page - 1, page, page + 1].filter((n) => n >= 1 && n <= totalPages))
  const sorted = [...set].sort((a, b) => a - b)
  const result: (number | '…')[] = []
  for (let i = 0; i < sorted.length; i++) {
    if (i > 0 && (sorted[i] as number) - (sorted[i - 1] as number) > 1) result.push('…')
    result.push(sorted[i])
  }
  return result
}
</script>

<template>
  <div v-if="totalPages > 1" class="flex items-center gap-1 text-xs justify-center py-3">
    <button
      class="px-2 py-1 rounded border border-gray-700 text-gray-400 hover:text-white hover:border-gray-500 disabled:opacity-30 transition-colors"
      :disabled="page <= 1"
      @click="emit('go', page - 1)"
    >‹</button>

    <template v-for="p in pages()" :key="p">
      <span v-if="p === '…'" class="px-1 text-gray-600">…</span>
      <button
        v-else
        class="min-w-[28px] py-1 rounded border transition-colors"
        :class="p === page
          ? 'border-blue-500/60 bg-blue-500/10 text-blue-400'
          : 'border-gray-700 text-gray-500 hover:text-white hover:border-gray-500'"
        @click="emit('go', p)"
      >{{ p }}</button>
    </template>

    <button
      class="px-2 py-1 rounded border border-gray-700 text-gray-400 hover:text-white hover:border-gray-500 disabled:opacity-30 transition-colors"
      :disabled="page >= totalPages"
      @click="emit('go', page + 1)"
    >›</button>

    <span class="ml-2 text-gray-600">共 {{ total }} 条</span>
  </div>
</template>
