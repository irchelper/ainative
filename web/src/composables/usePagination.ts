import { computed, ref, watch } from 'vue'

export function usePagination<T>(source: () => T[], pageSize = 20) {
  const page = ref(1)
  const total = computed(() => source().length)
  const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))
  const items = computed<T[]>(() => {
    const start = (page.value - 1) * pageSize
    return source().slice(start, start + pageSize)
  })

  // Reset to page 1 whenever source length changes
  watch(total, () => {
    if (page.value > totalPages.value) page.value = 1
  })

  function goTo(n: number) {
    page.value = Math.min(Math.max(1, n), totalPages.value)
  }
  function next() { goTo(page.value + 1) }
  function prev() { goTo(page.value - 1) }

  return { page, pageSize, total, items, totalPages, goTo, next, prev }
}
