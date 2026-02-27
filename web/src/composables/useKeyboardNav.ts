import { ref, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import type { Task } from '@/types'

export function useKeyboardNav(getVisibleTasks: () => Task[], onPatch?: (id: string, data: Record<string, unknown>) => Promise<void>) {
  const router = useRouter()
  const selectedIndex = ref(-1)
  const showHelp = ref(false)

  function isInputFocused(): boolean {
    const el = document.activeElement
    if (!el) return false
    const tag = el.tagName.toLowerCase()
    return tag === 'input' || tag === 'textarea' || tag === 'select' || (el as HTMLElement).isContentEditable
  }

  function clamp(i: number, len: number): number {
    if (len === 0) return -1
    return Math.max(0, Math.min(i, len - 1))
  }

  function onKeyDown(e: KeyboardEvent) {
    if (isInputFocused()) return

    const tasks = getVisibleTasks()
    const len = tasks.length

    switch (e.key) {
      case 'j':
      case 'ArrowDown':
        e.preventDefault()
        selectedIndex.value = clamp(selectedIndex.value + 1, len)
        scrollToSelected()
        break

      case 'k':
      case 'ArrowUp':
        e.preventDefault()
        selectedIndex.value = clamp(Math.max(0, selectedIndex.value - 1), len)
        scrollToSelected()
        break

      case 'Enter':
        if (selectedIndex.value >= 0 && selectedIndex.value < len) {
          router.push(`/tasks/${tasks[selectedIndex.value].id}`)
        }
        break

      case 'd':
        if (selectedIndex.value >= 0 && selectedIndex.value < len && onPatch) {
          const task = tasks[selectedIndex.value]
          onPatch(task.id, { status: 'done', version: task.version })
        }
        break

      case 'Escape':
        selectedIndex.value = -1
        showHelp.value = false
        break

      case '?':
        showHelp.value = !showHelp.value
        break
    }
  }

  function scrollToSelected() {
    const el = document.querySelector(`[data-keyboard-index="${selectedIndex.value}"]`)
    el?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  }

  onMounted(() => window.addEventListener('keydown', onKeyDown))
  onUnmounted(() => window.removeEventListener('keydown', onKeyDown))

  return { selectedIndex, showHelp }
}
