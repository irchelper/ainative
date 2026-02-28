<script setup lang="ts">
import { ref, computed, watch, onMounted, onUnmounted, nextTick } from 'vue'
import { useRouter } from 'vue-router'
import * as dagre from 'dagre'
import { select } from 'd3-selection'
import { zoom as d3zoom, zoomIdentity } from 'd3-zoom'
import type { Task } from '@/types'

// ─── Props ───────────────────────────────────────────────────────────────────
const props = withDefaults(defineProps<{
  tasks: Task[]
  loading?: boolean
}>(), {
  loading: false,
})

const router = useRouter()

// ─── Constants ───────────────────────────────────────────────────────────────
const NODE_W = 160
const NODE_H = 72
const RANK_SEP = 80
const NODE_SEP = 24

// ─── Status colors ───────────────────────────────────────────────────────────
const STATUS_FILL: Record<string, string> = {
  pending:     '#374151', // gray-700
  claimed:     '#1e3a5f', // dark blue
  in_progress: '#1e3a8a', // blue-900
  review:      '#3b0764', // purple-900
  done:        '#14532d', // green-900
  failed:      '#7f1d1d', // red-900
  blocked:     '#431407', // orange-900
  cancelled:   '#1f2937', // gray-800
}
const STATUS_STROKE: Record<string, string> = {
  pending:     '#6b7280',
  claimed:     '#60a5fa',
  in_progress: '#3b82f6',
  review:      '#a855f7',
  done:        '#22c55e',
  failed:      '#ef4444',
  blocked:     '#f97316',
  cancelled:   '#374151',
}
const STATUS_TEXT: Record<string, string> = {
  pending:     '#9ca3af',
  claimed:     '#93c5fd',
  in_progress: '#60a5fa',
  review:      '#c084fc',
  done:        '#4ade80',
  failed:      '#f87171',
  blocked:     '#fb923c',
  cancelled:   '#6b7280',
}

function fillFor(s: string)   { return STATUS_FILL[s]   ?? STATUS_FILL.pending }
function strokeFor(s: string) { return STATUS_STROKE[s] ?? STATUS_STROKE.pending }
function textFor(s: string)   { return STATUS_TEXT[s]   ?? STATUS_TEXT.pending }

// ─── dagre layout ────────────────────────────────────────────────────────────
interface LayoutNode {
  id: string
  x: number
  y: number
  width: number
  height: number
  task: Task
}
interface LayoutEdge {
  id: string
  points: { x: number; y: number }[]
}

const layout = computed<{ nodes: LayoutNode[]; edges: LayoutEdge[]; width: number; height: number }>(() => {
  if (!props.tasks.length) return { nodes: [], edges: [], width: 0, height: 0 }

  const g = new dagre.graphlib.Graph()
  g.setGraph({ rankdir: 'LR', ranksep: RANK_SEP, nodesep: NODE_SEP, marginx: 32, marginy: 32 })
  g.setDefaultEdgeLabel(() => ({}))

  const idSet = new Set(props.tasks.map((t) => t.id))

  for (const t of props.tasks) {
    g.setNode(t.id, { label: t.id, width: NODE_W, height: NODE_H })
  }

  for (const t of props.tasks) {
    for (const dep of t.depends_on ?? []) {
      if (idSet.has(dep)) {
        g.setEdge(dep, t.id)
      }
    }
  }

  dagre.layout(g)

  const taskMap = new Map(props.tasks.map((t) => [t.id, t]))
  const nodes: LayoutNode[] = g.nodes().map((id: string) => {
    const n = g.node(id)
    return { id, x: n.x, y: n.y, width: NODE_W, height: NODE_H, task: taskMap.get(id)! }
  }).filter((n: LayoutNode) => n.task)

  const edges: LayoutEdge[] = g.edges().map((e: { v: string; w: string }) => {
    const ed = g.edge(e)
    return { id: `${e.v}->${e.w}`, points: ed.points }
  })

  const gData = g.graph() as { width?: number; height?: number }
  return {
    nodes,
    edges,
    width: (gData.width ?? 0) + 64,
    height: (gData.height ?? 0) + 64,
  }
})

// ─── SVG path for edge ───────────────────────────────────────────────────────
function edgePath(pts: { x: number; y: number }[]): string {
  if (pts.length < 2) return ''
  const d: string[] = [`M ${pts[0].x} ${pts[0].y}`]
  // Use bezier curves through dagre's waypoints
  for (let i = 1; i < pts.length; i++) {
    const prev = pts[i - 1]
    const curr = pts[i]
    const mx = (prev.x + curr.x) / 2
    d.push(`C ${mx} ${prev.y}, ${mx} ${curr.y}, ${curr.x} ${curr.y}`)
  }
  return d.join(' ')
}

// ─── Tooltip ─────────────────────────────────────────────────────────────────
const tooltip = ref<{ visible: boolean; x: number; y: number; task: Task | null }>({
  visible: false,
  x: 0,
  y: 0,
  task: null,
})

function showTooltip(e: MouseEvent, task: Task) {
  tooltip.value = { visible: true, x: e.offsetX + 12, y: e.offsetY - 8, task }
}
function hideTooltip() {
  tooltip.value.visible = false
}

// ─── d3-zoom ─────────────────────────────────────────────────────────────────
const svgRef = ref<SVGSVGElement | null>(null)
const gRef = ref<SVGGElement | null>(null)
// eslint-disable-next-line @typescript-eslint/no-explicit-any
let zoomBehavior: any = null

function initZoom() {
  if (!svgRef.value || !gRef.value) return
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const svg = select(svgRef.value as any)
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  const g = select(gRef.value as any)

  zoomBehavior = d3zoom()
    .scaleExtent([0.2, 3])
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    .on('zoom', (event: any) => {
      g.attr('transform', event.transform.toString())
    })

  svg.call(zoomBehavior)
}

function resetZoom() {
  if (!svgRef.value || !zoomBehavior) return
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  ;(select(svgRef.value as any) as any)
    .transition()
    .duration(400)
    .call(zoomBehavior.transform, zoomIdentity)
}

onMounted(() => {
  nextTick(initZoom)
})
onUnmounted(() => {
  if (svgRef.value) {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    select(svgRef.value as any).on('.zoom', null)
  }
})

watch(() => layout.value, () => {
  nextTick(() => {
    if (!svgRef.value || !zoomBehavior) return
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    ;(select(svgRef.value as any) as any).call(zoomBehavior)
  })
})

// ─── Text wrapping helper ─────────────────────────────────────────────────────
function wrapTitle(title: string, maxLen = 24): string[] {
  if (title.length <= maxLen) return [title]
  // try word-break
  const words = title.split(/\s+/)
  const lines: string[] = []
  let current = ''
  for (const w of words) {
    if ((current + ' ' + w).trim().length <= maxLen) {
      current = (current + ' ' + w).trim()
    } else {
      if (current) lines.push(current)
      current = w.length > maxLen ? w.slice(0, maxLen - 1) + '…' : w
    }
  }
  if (current) lines.push(current)
  return lines.slice(0, 2) // max 2 lines
}
</script>

<template>
  <div class="relative w-full h-full flex flex-col">
    <!-- Loading -->
    <div v-if="loading" class="flex-1 flex items-center justify-center text-gray-600">
      <span class="text-2xl mr-3 animate-spin">⟳</span> 加载中…
    </div>

    <!-- Empty -->
    <div v-else-if="!tasks.length" class="flex-1 flex flex-col items-center justify-center text-gray-600">
      <div class="text-4xl mb-3">🕸</div>
      <div class="text-sm">暂无任务数据</div>
    </div>

    <!-- DAG SVG -->
    <div v-else class="relative flex-1 overflow-hidden">
      <!-- Controls -->
      <div class="absolute top-2 right-2 z-10 flex gap-1">
        <button
          class="bg-gray-800 border border-gray-700 text-gray-400 hover:text-white text-xs px-2 py-1 rounded-lg transition-colors"
          title="复位视图"
          @click="resetZoom"
        >⊙ 复位</button>
      </div>

      <!-- SVG canvas -->
      <svg
        ref="svgRef"
        class="w-full h-full cursor-grab active:cursor-grabbing select-none"
        :viewBox="`0 0 ${layout.width || 400} ${layout.height || 300}`"
        preserveAspectRatio="xMidYMid meet"
      >
        <defs>
          <!-- Arrow marker -->
          <marker id="dag-arrow" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto" markerUnits="strokeWidth">
            <path d="M0,0 L0,6 L8,3 z" fill="#4b5563" />
          </marker>
        </defs>

        <g ref="gRef">
          <!-- Edges -->
          <g class="dag-edges">
            <path
              v-for="edge in layout.edges"
              :key="edge.id"
              :d="edgePath(edge.points)"
              fill="none"
              stroke="#4b5563"
              stroke-width="1.5"
              stroke-dasharray="none"
              marker-end="url(#dag-arrow)"
            />
          </g>

          <!-- Nodes -->
          <g class="dag-nodes">
            <g
              v-for="node in layout.nodes"
              :key="node.id"
              :transform="`translate(${node.x - NODE_W / 2}, ${node.y - NODE_H / 2})`"
              class="dag-node cursor-pointer"
              @click="router.push(`/tasks/${node.id}`)"
              @mouseenter="(e) => showTooltip(e, node.task)"
              @mouseleave="hideTooltip"
            >
              <!-- Node rect -->
              <rect
                :width="NODE_W"
                :height="NODE_H"
                rx="8"
                :fill="fillFor(node.task.status)"
                :stroke="strokeFor(node.task.status)"
                stroke-width="1.5"
                class="transition-all hover:brightness-125"
              />
              <!-- Status dot + label -->
              <circle
                cx="12"
                cy="14"
                r="4"
                :fill="strokeFor(node.task.status)"
              />
              <text
                x="22"
                y="18"
                font-size="9"
                :fill="textFor(node.task.status)"
                font-family="monospace"
                text-anchor="start"
                dominant-baseline="middle"
              >{{ node.task.status.toUpperCase() }}</text>

              <!-- Title lines -->
              <text
                v-for="(line, li) in wrapTitle(node.task.title)"
                :key="li"
                :x="NODE_W / 2"
                :y="32 + li * 14"
                font-size="11"
                fill="#e5e7eb"
                font-family="system-ui, sans-serif"
                font-weight="500"
                text-anchor="middle"
                dominant-baseline="middle"
              >{{ line }}</text>

              <!-- Agent badge -->
              <text
                :x="NODE_W / 2"
                :y="NODE_H - 10"
                font-size="9"
                fill="#6b7280"
                font-family="monospace"
                text-anchor="middle"
                dominant-baseline="middle"
              >{{ node.task.assigned_to === 'human' ? '👤' : '🤖' }} {{ node.task.assigned_to }}</text>
            </g>
          </g>
        </g>
      </svg>

      <!-- Tooltip -->
      <div
        v-if="tooltip.visible && tooltip.task"
        class="absolute z-20 pointer-events-none bg-gray-900 border border-gray-700 rounded-xl shadow-2xl p-3 max-w-64 text-xs"
        :style="{ left: tooltip.x + 'px', top: tooltip.y + 'px' }"
      >
        <div class="font-semibold text-gray-100 mb-1 leading-snug">{{ tooltip.task.title }}</div>
        <div class="text-gray-500 mb-0.5">{{ tooltip.task.id.slice(0, 16) }}…</div>
        <div class="flex gap-2 mt-1.5">
          <span class="px-1.5 py-0.5 rounded" :style="{ background: fillFor(tooltip.task.status), color: strokeFor(tooltip.task.status), border: `1px solid ${strokeFor(tooltip.task.status)}` }">
            {{ tooltip.task.status }}
          </span>
          <span class="text-gray-500">{{ tooltip.task.assigned_to }}</span>
        </div>
        <div v-if="tooltip.task.depends_on?.length" class="mt-1.5 text-gray-600">
          依赖 {{ tooltip.task.depends_on.length }} 个任务
        </div>
        <div class="mt-1.5 text-blue-400">点击查看详情 →</div>
      </div>
    </div>
  </div>
</template>
