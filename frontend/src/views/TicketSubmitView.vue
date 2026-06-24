<template>
  <div class="ticket-page">
    <!-- 流星层：纯装饰，屏幕阅读器忽略 -->
    <div class="meteor-sky" aria-hidden="true">
      <span v-for="n in meteorCount" :key="n" class="meteor" :style="meteorStyle(n)" />
    </div>
    <!-- 光标光晕：跟随鼠标，触摸设备 / 减弱动效时不渲染 -->
    <div v-if="glowEnabled" ref="cursorGlow" class="cursor-glow" aria-hidden="true" />

    <div class="ticket-shell">
      <!-- 顶部部门品牌条 -->
      <header class="ticket-brand-bar">
        <span class="brand-mark" aria-hidden="true">
          <icon-storage />
        </span>
        <div class="brand-text">
          <span class="brand-dept">基础设施研发部</span>
          <span class="brand-tagline">Infrastructure R&amp;D · 数据库迁移平台</span>
        </div>
      </header>

      <div class="ticket-card glass">
      <div class="ticket-header">
        <h1 class="ticket-title">迁移工单申请</h1>
        <p class="ticket-subtitle">填写源数据库与目标数据库的连接信息，提交后由管理员处理。</p>
      </div>

      <a-result v-if="submitted" status="success" title="提交成功" subtitle="您的迁移工单已提交，管理员将尽快处理。">
        <template #extra>
          <a-button type="primary" @click="resetForm">再提交一个</a-button>
        </template>
      </a-result>

      <a-form v-else :model="form" layout="vertical" ref="formRef">
        <a-row :gutter="16">
          <a-col :span="12">
            <a-form-item label="申请人" field="applicant">
              <a-input v-model="form.applicant" placeholder="姓名 / 邮箱 / 工号（选填）" />
            </a-form-item>
          </a-col>
        </a-row>
        <a-form-item label="需求说明" field="remark">
          <a-textarea v-model="form.remark" placeholder="迁移背景、期望时间等（选填）" :auto-size="{ minRows: 2, maxRows: 4 }" />
        </a-form-item>

        <a-divider orientation="left">源数据库</a-divider>
        <a-alert type="normal" class="ticket-tip">
          若已有现成的源库连接，请选择「提供连接信息」并填写；若没有现成连接、只有离线导出的 .sql / .dmp 文件，请选择「上传离线文件」直接上传。
        </a-alert>
        <a-form-item label="源库提供方式">
          <a-radio-group v-model="srcMode" type="button">
            <a-radio value="connection">提供连接信息</a-radio>
            <a-radio value="file">上传离线文件（.sql / .dmp）</a-radio>
          </a-radio-group>
        </a-form-item>
        <a-form-item label="数据库类型" field="src_db_type" :rules="[{ required: true, message: '请选择数据库类型' }]">
          <a-select v-model="form.src_db_type" placeholder="选择数据库类型">
            <a-option v-for="o in DB_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</a-option>
          </a-select>
        </a-form-item>

        <template v-if="srcMode === 'connection'">
          <a-row :gutter="12">
            <a-col :span="16">
              <a-form-item label="主机" field="src_host" :rules="[{ required: true, message: '请输入主机' }]">
                <a-input v-model="form.src_host" placeholder="localhost" />
              </a-form-item>
            </a-col>
            <a-col :span="8">
              <a-form-item label="端口" field="src_port" :rules="[{ required: true, message: '请输入端口' }]">
                <a-input-number v-model="form.src_port" :min="1" :max="65535" style="width: 100%" />
              </a-form-item>
            </a-col>
          </a-row>
          <a-form-item label="数据库名" field="src_database" :rules="[{ required: srcDbNameRequired, message: '请输入数据库名' }]">
            <a-input v-model="form.src_database" placeholder="数据库名" />
          </a-form-item>
          <a-form-item label="用户名" field="src_username" :rules="[{ required: true, message: '请输入用户名' }]">
            <a-input v-model="form.src_username" placeholder="用户名" />
          </a-form-item>
          <a-form-item label="密码" field="src_password" :rules="[{ required: true, message: '请输入密码' }]">
            <a-input-password v-model="form.src_password" placeholder="密码" />
          </a-form-item>
        </template>

        <template v-else>
          <a-form-item label="离线文件" extra="支持 .sql / .dmp，单文件最大 50GB。上传时间取决于文件大小与网络，上传过程中请勿关闭页面。">
            <div class="upload-block">
              <a-upload
                :auto-upload="false"
                accept=".sql,.dmp"
                :limit="1"
                :show-file-list="false"
                :file-list="fileList"
                @change="handleFileChange"
              >
                <template #upload-button>
                  <a-button :disabled="uploading">
                    <template #icon><icon-upload /></template>
                    选择文件
                  </a-button>
                </template>
              </a-upload>
              <span v-if="form.src_file_path" class="upload-done">
                <icon-file /> {{ form.src_file_name }}（{{ formatBytes(form.src_file_size || 0) }}）
                <a-button type="text" size="mini" status="danger" :disabled="uploading" @click="clearFile">
                  <template #icon><icon-delete /></template>
                  移除
                </a-button>
              </span>
            </div>
            <a-progress v-if="uploading" :percent="uploadPercent / 100" :show-text="true" style="margin-top: 8px" />
          </a-form-item>
        </template>

        <a-divider orientation="left">目标数据库</a-divider>
        <a-alert type="normal" class="ticket-tip">
          目标库信息可按实际情况填写：已有现成环境的，请填写连接信息以便直接迁移；尚未准备的，相关字段可留空，我们会在受理工单时为您协助创建。
        </a-alert>
        <a-form-item label="数据库类型" field="dst_db_type" :rules="[{ required: true, message: '请选择数据库类型' }]">
          <a-select v-model="form.dst_db_type" placeholder="选择数据库类型">
            <a-option v-for="o in DB_OPTIONS" :key="o.value" :value="o.value">{{ o.label }}</a-option>
          </a-select>
        </a-form-item>
        <a-row :gutter="12">
          <a-col :span="16">
            <a-form-item label="主机" field="dst_host" extra="若已有现成目标库主机，请填写其地址；若暂未准备，可留空。">
              <a-input v-model="form.dst_host" placeholder="localhost（选填）" />
            </a-form-item>
          </a-col>
          <a-col :span="8">
            <a-form-item label="端口" field="dst_port">
              <a-input-number v-model="form.dst_port" :min="1" :max="65535" style="width: 100%" />
            </a-form-item>
          </a-col>
        </a-row>
        <a-form-item label="数据库名（如需新建目标库，请填写期望的库名）" field="dst_database">
          <a-input v-model="form.dst_database" placeholder="已有或期望新建的数据库名（选填）" />
        </a-form-item>
        <a-form-item label="用户名" field="dst_username" extra="若已有可用账号，请填写其用户名；若暂未准备，可留空。">
          <a-input v-model="form.dst_username" placeholder="用户名（选填）" />
        </a-form-item>
        <a-form-item label="密码" field="dst_password" extra="若已有账号密码，请填写；若暂未准备，可留空。">
          <a-input-password v-model="form.dst_password" placeholder="密码（选填）" />
        </a-form-item>

        <a-form-item label="验证码" field="captcha_code" :rules="[{ required: true, message: '请输入验证码' }]">
          <div class="ticket-captcha">
            <a-input
              v-model="form.captcha_code"
              placeholder="请输入图中字符"
              allow-clear
              @press-enter="handleSubmit"
            />
            <img
              v-if="captchaImg"
              class="captcha-img"
              :src="captchaImg"
              alt="验证码"
              title="点击刷新验证码"
              @click="loadCaptcha"
            />
            <a-spin v-else class="captcha-img" />
          </div>
        </a-form-item>

        <div class="ticket-actions">
          <a-button type="primary" size="large" long :loading="submitting" @click="handleSubmit">
            提交工单
          </a-button>
        </div>
      </a-form>
      </div>

      <!-- 底部署名 -->
      <footer class="ticket-footer">
        <span class="footer-line">Powered by <strong>基础设施研发部</strong></span>
        <span class="footer-sub">© {{ year }} Infrastructure R&amp;D · 数据库迁移平台</span>
      </footer>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, reactive, watch, computed, onMounted, onBeforeUnmount } from 'vue'
import { Message } from '@arco-design/web-vue'
import type { FileItem } from '@arco-design/web-vue'
import { submitTicket, uploadTicketFile, getCaptcha, type TicketForm } from '@/api/tickets'

const year = new Date().getFullYear()

// ─── 背景特效：流星 + 光标光晕 ───
// 减弱动效 / 触摸设备时关闭，避免无意义动画与移动端卡顿
const reduceMotion =
  typeof window !== 'undefined' &&
  window.matchMedia?.('(prefers-reduced-motion: reduce)').matches
const isTouch =
  typeof window !== 'undefined' && window.matchMedia?.('(hover: none)').matches

const meteorCount = reduceMotion ? 0 : 7
const glowEnabled = ref(!reduceMotion && !isTouch)

// 为每条流星生成错开的起点、时长与延迟，营造“时不时划过”的节奏
function meteorStyle(n: number) {
  const seed = (n * 9301 + 49297) % 233280
  const rand = seed / 233280
  return {
    top: `${Math.round(rand * 50)}%`,
    left: `${Math.round(10 + rand * 80)}%`,
    animationDelay: `${(rand * 8).toFixed(2)}s`,
    animationDuration: `${(3 + rand * 3).toFixed(2)}s`,
  }
}

const cursorGlow = ref<HTMLElement | null>(null)
let rafId = 0
let pointerX = 0
let pointerY = 0

function onPointerMove(e: MouseEvent) {
  pointerX = e.clientX
  pointerY = e.clientY
  if (rafId) return
  rafId = requestAnimationFrame(() => {
    rafId = 0
    if (cursorGlow.value) {
      cursorGlow.value.style.transform = `translate3d(${pointerX}px, ${pointerY}px, 0) translate(-50%, -50%)`
    }
  })
}

onMounted(() => {
  if (glowEnabled.value) window.addEventListener('mousemove', onPointerMove, { passive: true })
  loadCaptcha()
})
onBeforeUnmount(() => {
  window.removeEventListener('mousemove', onPointerMove)
  if (rafId) cancelAnimationFrame(rafId)
})

const DB_OPTIONS = [
  { value: 'mysql', label: 'MySQL' },
  { value: 'postgres', label: 'PostgreSQL' },
  { value: 'oracle', label: 'Oracle' },
  { value: 'sqlserver', label: 'SQL Server' },
  { value: 'gaussdb', label: 'GaussDB' },
  { value: 'dameng', label: 'DaMeng（达梦）' },
  { value: 'seabox', label: 'SeaBox' },
  { value: 'highgo', label: 'HighGo（瀚高）' },
]

const defaultPortMap: Record<string, number> = {
  mysql: 3306,
  postgres: 5432,
  oracle: 1521,
  sqlserver: 1433,
  gaussdb: 5432,
  dameng: 5236,
  seabox: 5432,
  highgo: 5866,
}

const defaultForm = (): TicketForm => ({
  applicant: '',
  remark: '',
  captcha_id: '',
  captcha_code: '',
  src_db_type: 'mysql',
  src_host: '',
  src_port: 3306,
  src_database: '',
  src_username: '',
  src_password: '',
  src_file_name: '',
  src_file_path: '',
  src_file_size: 0,
  dst_db_type: 'postgres',
  dst_host: '',
  dst_port: 5432,
  dst_database: '',
  dst_username: '',
  dst_password: '',
})

const form = reactive(defaultForm())
const formRef = ref<{ validate: () => Promise<Record<string, unknown> | undefined> } | null>(null)
const submitting = ref(false)
const submitted = ref(false)

// 图形验证码：captchaImg 为后端签发的 base64 图片，form.captcha_id 随表单一并提交
const captchaImg = ref('')
async function loadCaptcha() {
  captchaImg.value = ''
  form.captcha_code = ''
  try {
    const { data } = await getCaptcha()
    form.captcha_id = data.captcha_id
    captchaImg.value = data.image
  } catch {
    Message.error('验证码加载失败，请点击图片重试')
  }
}

// 源库提供方式：connection（填连接信息）/ file（上传离线文件）
const srcMode = ref<'connection' | 'file'>('connection')
const fileList = ref<FileItem[]>([])
const uploading = ref(false)
const uploadPercent = ref(0)

// MySQL / 达梦 无需库名，其余必填（仅源库强制；目标库全部选填）
const srcDbNameRequired = computed(() => form.src_db_type !== 'mysql' && form.src_db_type !== 'dameng')

// 选择数据库类型时自动填默认端口
watch(() => form.src_db_type, (t) => { form.src_port = defaultPortMap[t] ?? 3306 })
watch(() => form.dst_db_type, (t) => { form.dst_port = defaultPortMap[t] ?? 3306 })

// 选中文件后立即上传（流式，支持 50GB），成功后把落盘信息写入表单
async function handleFileChange(list: FileItem[], item: FileItem) {
  fileList.value = list.slice(-1)
  const file = item.file
  if (!file) return
  uploading.value = true
  uploadPercent.value = 0
  try {
    const { data } = await uploadTicketFile(file, (p) => { uploadPercent.value = p })
    form.src_file_name = data.original_name
    form.src_file_path = data.stored_path
    form.src_file_size = data.size
    Message.success('文件上传成功')
  } catch (e: any) {
    fileList.value = []
    const detail = e?.response?.data?.error
    Message.error(detail ? `上传失败：${detail}` : '上传失败')
  } finally {
    uploading.value = false
  }
}

function clearFile() {
  fileList.value = []
  form.src_file_name = ''
  form.src_file_path = ''
  form.src_file_size = 0
  uploadPercent.value = 0
}

// formatBytes 把字节数格式化为人类可读大小
function formatBytes(bytes: number): string {
  if (bytes <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const i = Math.floor(Math.log(bytes) / Math.log(1024))
  return `${(bytes / Math.pow(1024, i)).toFixed(i === 0 ? 0 : 2)} ${units[i]}`
}

async function handleSubmit() {
  if (uploading.value) {
    Message.warning('文件正在上传，请等待上传完成')
    return
  }
  if (srcMode.value === 'file' && !form.src_file_path) {
    Message.warning('请先上传源库离线文件')
    return
  }
  const valid = await formRef.value?.validate()
  if (valid) return
  submitting.value = true
  try {
    await submitTicket(form)
    submitted.value = true
  } catch (e: any) {
    if (e?.response?.status === 429) {
      Message.error('提交过于频繁，请稍后再试')
    } else {
      const detail = e?.response?.data?.error
      Message.error(detail ? `提交失败：${detail}` : '提交失败')
    }
    // 验证码单次有效，提交失败后刷新一张新的
    loadCaptcha()
  } finally {
    submitting.value = false
  }
}

function resetForm() {
  Object.assign(form, defaultForm())
  srcMode.value = 'connection'
  clearFile()
  submitted.value = false
  loadCaptcha()
}
</script>

<style scoped>
/* ─── 深空背景容器 ─── */
.ticket-page {
  position: relative;
  min-height: 100vh;
  min-height: 100dvh;
  padding: 48px 16px;
  display: flex;
  flex-direction: column;
  align-items: center;
  overflow: hidden;
  background:
    radial-gradient(1200px 600px at 80% -10%, rgba(37, 99, 235, 0.18), transparent 60%),
    radial-gradient(900px 500px at 10% 110%, rgba(16, 185, 129, 0.14), transparent 60%),
    linear-gradient(160deg, #0b1026 0%, #131a3a 45%, #1e293b 100%);
}
/* 星点底纹（伪元素，不影响布局）——两层错相闪烁 */
.ticket-page::before,
.ticket-page::after {
  content: '';
  position: absolute;
  inset: 0;
  background-repeat: no-repeat;
  pointer-events: none;
  will-change: opacity;
}
.ticket-page::before {
  background-image:
    radial-gradient(1.5px 1.5px at 20% 30%, rgba(255, 255, 255, 0.7), transparent),
    radial-gradient(1.5px 1.5px at 70% 20%, rgba(255, 255, 255, 0.5), transparent),
    radial-gradient(1px 1px at 40% 70%, rgba(255, 255, 255, 0.6), transparent),
    radial-gradient(1px 1px at 85% 60%, rgba(255, 255, 255, 0.45), transparent),
    radial-gradient(1.5px 1.5px at 55% 45%, rgba(255, 255, 255, 0.5), transparent),
    radial-gradient(1px 1px at 10% 85%, rgba(255, 255, 255, 0.4), transparent);
  opacity: 0.6;
  animation: star-twinkle 3.4s ease-in-out infinite;
}
/* 第二层星点：位置错开、节奏更慢且反相，叠加出自然闪烁 */
.ticket-page::after {
  background-image:
    radial-gradient(1px 1px at 32% 18%, rgba(255, 255, 255, 0.65), transparent),
    radial-gradient(1.5px 1.5px at 60% 75%, rgba(191, 219, 254, 0.6), transparent),
    radial-gradient(1px 1px at 88% 35%, rgba(255, 255, 255, 0.5), transparent),
    radial-gradient(1.5px 1.5px at 15% 55%, rgba(255, 255, 255, 0.45), transparent),
    radial-gradient(1px 1px at 75% 12%, rgba(191, 219, 254, 0.55), transparent),
    radial-gradient(1px 1px at 48% 92%, rgba(255, 255, 255, 0.4), transparent);
  opacity: 0.45;
  animation: star-twinkle 4.8s ease-in-out infinite;
  animation-delay: -1.6s;
}
@keyframes star-twinkle {
  0%,
  100% {
    opacity: 0.25;
  }
  50% {
    opacity: 0.7;
  }
}

/* ─── 流星层 ─── */
.meteor-sky {
  position: fixed;
  inset: 0;
  pointer-events: none;
  z-index: 0;
}
.meteor {
  position: absolute;
  width: 2px;
  height: 2px;
  border-radius: 50%;
  background: #fff;
  box-shadow: 0 0 6px 1px rgba(255, 255, 255, 0.7);
  opacity: 0;
  transform: rotate(35deg);
  animation-name: meteor-fall;
  animation-timing-function: ease-in;
  animation-iteration-count: infinite;
}
/* 拖尾 */
.meteor::after {
  content: '';
  position: absolute;
  top: 50%;
  right: 0;
  width: 120px;
  height: 1px;
  transform: translateY(-50%);
  background: linear-gradient(90deg, rgba(255, 255, 255, 0.85), transparent);
}
@keyframes meteor-fall {
  0% {
    opacity: 0;
    transform: rotate(35deg) translate3d(0, 0, 0);
  }
  10% {
    opacity: 1;
  }
  70% {
    opacity: 1;
  }
  100% {
    opacity: 0;
    transform: rotate(35deg) translate3d(-340px, 240px, 0);
  }
}

/* ─── 光标光晕 ─── */
.cursor-glow {
  position: fixed;
  top: 0;
  left: 0;
  width: 380px;
  height: 380px;
  border-radius: 50%;
  background: radial-gradient(
    circle,
    rgba(34, 197, 94, 0.18) 0%,
    rgba(37, 99, 235, 0.12) 35%,
    transparent 70%
  );
  pointer-events: none;
  mix-blend-mode: screen;
  will-change: transform;
  transition: transform 120ms cubic-bezier(0.22, 1, 0.36, 1);
  z-index: 1;
}

/* ─── 内容 shell ─── */
.ticket-shell {
  position: relative;
  z-index: 2;
  width: 100%;
  max-width: 720px;
  display: flex;
  flex-direction: column;
  align-items: stretch;
}

/* ─── 顶部部门品牌条 ─── */
.ticket-brand-bar {
  display: flex;
  align-items: center;
  gap: 14px;
  margin-bottom: 22px;
  padding: 0 4px;
}
.brand-mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 44px;
  height: 44px;
  border-radius: 12px;
  font-size: 22px;
  color: #fff;
  background: linear-gradient(135deg, #22c55e, #2563eb);
  box-shadow: 0 6px 18px rgba(37, 99, 235, 0.4);
}
.brand-text {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.brand-dept {
  font-size: 20px;
  font-weight: 700;
  letter-spacing: 0.5px;
  background: linear-gradient(90deg, #e2e8f0, #ffffff 40%, #93c5fd);
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
}
.brand-tagline {
  font-size: 12px;
  color: rgba(226, 232, 240, 0.7);
  letter-spacing: 0.3px;
}

/* ─── 玻璃卡 ─── */
.ticket-card.glass {
  width: 100%;
  background: rgba(255, 255, 255, 0.94);
  -webkit-backdrop-filter: blur(20px) saturate(140%);
  backdrop-filter: blur(20px) saturate(140%);
  border: 1px solid rgba(255, 255, 255, 0.6);
  border-radius: 18px;
  box-shadow:
    0 24px 60px rgba(8, 12, 30, 0.45),
    0 2px 8px rgba(8, 12, 30, 0.2),
    inset 0 1px 0 rgba(255, 255, 255, 0.8);
  padding: 32px 36px;
  animation: card-in 380ms cubic-bezier(0.22, 1, 0.36, 1);
}
@keyframes card-in {
  from {
    opacity: 0;
    transform: translateY(16px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}
.ticket-header {
  margin-bottom: 24px;
}
.ticket-title {
  font-size: 22px;
  font-weight: 700;
  color: #0f172a;
  margin: 0 0 6px;
}
.ticket-subtitle {
  font-size: 13px;
  color: #64748b;
  margin: 0;
}
.ticket-actions {
  margin-top: 24px;
}
.ticket-captcha {
  display: flex;
  align-items: center;
  gap: 12px;
  width: 100%;
}
.ticket-captcha .captcha-img {
  height: 36px;
  width: 108px;
  flex-shrink: 0;
  border-radius: 4px;
  cursor: pointer;
  object-fit: cover;
  background: rgba(255, 255, 255, 0.06);
}
.ticket-tip {
  margin-bottom: 16px;
}
.upload-block {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
}
.upload-done {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: #1d2129;
  font-size: 13px;
}

/* ─── 底部署名 ─── */
.ticket-footer {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  margin-top: 28px;
  padding-top: 18px;
  text-align: center;
  border-top: 1px solid rgba(148, 163, 184, 0.18);
  width: 100%;
}
.footer-line {
  font-size: 13px;
  color: rgba(226, 232, 240, 0.85);
  letter-spacing: 0.3px;
}
.footer-line strong {
  color: #fff;
  font-weight: 600;
}
.footer-sub {
  font-size: 11px;
  color: rgba(148, 163, 184, 0.65);
  font-family: var(--font-mono, monospace);
}

/* ─── 响应式 ─── */
@media (max-width: 768px) {
  .ticket-page {
    padding: 32px 12px;
  }
  .ticket-card.glass {
    padding: 24px 18px;
  }
  .brand-dept {
    font-size: 18px;
  }
  /* 移动端减少流星，降低开销 */
  .meteor:nth-child(n + 4) {
    display: none;
  }
}

/* ─── 减弱动效：关停所有装饰动画 ─── */
@media (prefers-reduced-motion: reduce) {
  .meteor {
    display: none;
  }
  .cursor-glow {
    display: none;
  }
  .ticket-card.glass {
    animation: none;
  }
}
</style>
