import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

const COMPACT_LAYOUT = '(max-width: 1100px)'
const INITIAL_EDITOR_RATIO = 0.44
const KEYBOARD_STEP = 24

export function useQueryWorkspace() {
  const workbenchRef = ref<HTMLElement | null>(null)
  const workspaceRef = ref<HTMLElement | null>(null)
  const compactLayout = window.matchMedia(COMPACT_LAYOUT)
  const sidebarOpen = ref(!compactLayout.matches)
  const availableHeight = ref(0)
  const editorRatio = ref(INITIAL_EDITOR_RATIO)
  let observer: ResizeObserver | undefined
  let dragEvents: AbortController | undefined

  // Keep both panes usable while leaving room to resize them in short windows.
  const minimumEditorHeight = computed(() => Math.min(180, availableHeight.value * 0.42))
  const minimumResultHeight = computed(() => Math.min(175, availableHeight.value * 0.42))
  const maximumEditorHeight = computed(() => availableHeight.value - minimumResultHeight.value)
  const editorPixels = computed(() => limitEditorHeight(availableHeight.value * editorRatio.value))
  const editorHeight = computed(() => availableHeight.value ? `${editorPixels.value}px` : undefined)
  const separatorValue = computed(() => heightPercentage(editorPixels.value, INITIAL_EDITOR_RATIO * 100))
  const separatorMin = computed(() => heightPercentage(minimumEditorHeight.value, 0))
  const separatorMax = computed(() => heightPercentage(maximumEditorHeight.value, 100))

  function limitEditorHeight(height: number) {
    return Math.min(Math.max(height, minimumEditorHeight.value), maximumEditorHeight.value)
  }

  function heightPercentage(height: number, fallback: number) {
    return availableHeight.value ? Math.round(height * 100 / availableHeight.value) : fallback
  }

  function measureWorkspace() {
    const workspace = workspaceRef.value
    if (!workspace) return
    const divider = workspace.querySelector<HTMLElement>('.workspace-divider')
    availableHeight.value = Math.max(0, workspace.clientHeight - (divider?.offsetHeight ?? 0))
  }

  function setEditorHeight(height: number) {
    if (availableHeight.value > 0) editorRatio.value = limitEditorHeight(height) / availableHeight.value
  }

  function endDrag() {
    dragEvents?.abort()
    dragEvents = undefined
  }

  function resizeOnPointer(event: PointerEvent) {
    const divider = event.currentTarget
    const workspace = workspaceRef.value
    if (event.button !== 0 || !(divider instanceof HTMLElement) || !workspace) return

    event.preventDefault()
    divider.focus({ preventScroll: true })
    measureWorkspace()
    endDrag()

    const pointerId = event.pointerId
    const pointerOffset = event.clientY - divider.getBoundingClientRect().top
    dragEvents = new AbortController()
    const options = { signal: dragEvents.signal }
    window.addEventListener('pointermove', (move) => {
      if (move.pointerId === pointerId) {
        setEditorHeight(move.clientY - workspace.getBoundingClientRect().top - pointerOffset)
      }
    }, options)
    const finish = (end: PointerEvent) => {
      if (end.pointerId === pointerId) endDrag()
    }
    window.addEventListener('pointerup', finish, options)
    window.addEventListener('pointercancel', finish, options)
    window.addEventListener('blur', endDrag, options)
  }

  function resizeOnKey(event: KeyboardEvent) {
    let height: number
    switch (event.key) {
      case 'ArrowUp':
        height = editorPixels.value - KEYBOARD_STEP
        break
      case 'ArrowDown':
        height = editorPixels.value + KEYBOARD_STEP
        break
      case 'Home':
        height = minimumEditorHeight.value
        break
      case 'End':
        height = maximumEditorHeight.value
        break
      default:
        return
    }
    event.preventDefault()
    setEditorHeight(height)
  }

  function updateSidebarDefault() {
    sidebarOpen.value = !compactLayout.matches
  }

  async function toggleSidebar() {
    sidebarOpen.value = !sidebarOpen.value
    if (!compactLayout.matches) return
    await nextTick()
    const selector = sidebarOpen.value ? '#query-object-panel' : '[aria-controls="query-object-panel"]'
    workbenchRef.value?.querySelector<HTMLElement>(selector)?.focus({ preventScroll: true })
  }

  function closeSidebarOnEscape(event: KeyboardEvent) {
    if (event.key !== 'Escape' || event.defaultPrevented || !compactLayout.matches || !sidebarOpen.value) return
    if (!(event.target instanceof Element) || !event.target.closest('#query-object-panel')) return
    event.preventDefault()
    void toggleSidebar()
  }

  onMounted(() => {
    compactLayout.addEventListener('change', updateSidebarDefault)
    workbenchRef.value?.addEventListener('keydown', closeSidebarOnEscape)

    measureWorkspace()
    observer = new ResizeObserver(measureWorkspace)
    if (workspaceRef.value) observer.observe(workspaceRef.value)
  })

  onBeforeUnmount(() => {
    endDrag()
    observer?.disconnect()
    compactLayout.removeEventListener('change', updateSidebarDefault)
    workbenchRef.value?.removeEventListener('keydown', closeSidebarOnEscape)
  })

  return {
    workbenchRef,
    workspaceRef,
    sidebarOpen,
    editorHeight,
    separatorValue,
    separatorMin,
    separatorMax,
    toggleSidebar,
    resizeOnPointer,
    resizeOnKey,
  }
}
