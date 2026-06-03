<template>
  <div class="login-page">
    <!-- 背景装饰层 -->
    <div class="bg-grid" aria-hidden="true"></div>
    <div class="bg-scan" aria-hidden="true"></div>
    <div class="bg-orb bg-orb-1" aria-hidden="true"></div>
    <div class="bg-orb bg-orb-2" aria-hidden="true"></div>
    <div class="bg-orb bg-orb-3" aria-hidden="true"></div>

    <!-- 登录卡片 -->
    <div class="login-card" role="main">
      <!-- Logo 区 -->
      <div class="logo-area">
        <div class="logo-title">
          <div class="plane-fly-zone" aria-hidden="true">
            <div class="meteor-trail"></div>
            <div class="meteor-head"></div>
          </div>
          <span class="logo-type" aria-label="DBGold">
            <span
              v-for="(char, i) in LOGO_TEXT"
              :key="i"
              class="logo-char"
              :style="{ animationDelay: `${i * -1}s` }"
            >{{ char }}</span>
          </span>
        </div>
        <p class="logo-sub">基础设施研发部数据迁移平台</p>
        <div class="logo-divider" aria-hidden="true"></div>
      </div>

      <!-- 表单 -->
      <a-form :model="form" layout="vertical" @submit="handleSubmit" class="login-form">
        <a-form-item
          label="用户名"
          field="username"
          :rules="[{ required: true, message: '请输入用户名' }]"
        >
          <a-input
            v-model="form.username"
            placeholder="请输入用户名"
            allow-clear
            autocomplete="username"
            class="tech-input"
          >
            <template #prefix>
              <icon-user class="input-icon" />
            </template>
          </a-input>
        </a-form-item>

        <a-form-item
          label="密码"
          field="password"
          :rules="[{ required: true, message: '请输入密码' }]"
        >
          <a-input-password
            v-model="form.password"
            placeholder="请输入密码"
            autocomplete="current-password"
            class="tech-input"
          >
            <template #prefix>
              <icon-lock class="input-icon" />
            </template>
          </a-input-password>
        </a-form-item>

        <a-form-item class="submit-item">
          <a-button
            type="primary"
            html-type="submit"
            long
            :loading="loading"
            class="login-btn"
          >
            <template v-if="!loading">
              <icon-arrow-right class="btn-icon" />
              登 录
            </template>
            <template v-else>
              验证中...
            </template>
          </a-button>
        </a-form-item>
      </a-form>

      <!-- 底部信息 -->
      <div class="card-footer">
        <span class="footer-tag">DBGold v2.0</span>
        <span class="footer-sep" aria-hidden="true">·</span>
        <span class="footer-tag">Secure Access</span>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Message } from '@arco-design/web-vue'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const auth = useAuthStore()
const loading = ref(false)
const form = reactive({ username: '', password: '' })

const LOGO_TEXT = 'DBGold'

/* ─── 登录逻辑 ─── */
async function handleSubmit({ errors }: { errors?: Record<string, unknown> }) {
  if (errors) return
  loading.value = true
  try {
    await auth.login(form.username, form.password)
    router.push('/connections')
  } catch {
    Message.error('用户名或密码错误，请重试')
  } finally {
    loading.value = false
  }
}
</script>

<style scoped>
/* ─── 页面容器 ─── */
.login-page {
  position: relative;
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  background:
    radial-gradient(ellipse 80% 60% at 20% 10%, rgba(79, 70, 229, 0.07) 0%, transparent 55%),
    radial-gradient(ellipse 60% 50% at 85% 90%, rgba(34, 197, 94, 0.06) 0%, transparent 50%),
    radial-gradient(ellipse 50% 40% at 60% 40%, rgba(56, 189, 248, 0.04) 0%, transparent 50%),
    #F1F5F9;
  overflow: hidden;
  font-family: 'Inter', sans-serif;
}

/* ─── 背景网格（深色线在浅底上）─── */
.bg-grid {
  position: absolute;
  inset: 0;
  background-image:
    linear-gradient(rgba(15, 23, 42, 0.04) 1px, transparent 1px),
    linear-gradient(90deg, rgba(15, 23, 42, 0.04) 1px, transparent 1px);
  background-size: 48px 48px;
  pointer-events: none;
}

/* ─── 扫描线（indigo 色）─── */
.bg-scan {
  position: absolute;
  inset: 0;
  pointer-events: none;
  overflow: hidden;
}
.bg-scan::after {
  content: '';
  position: absolute;
  left: 0;
  right: 0;
  height: 2px;
  background: linear-gradient(90deg, transparent, rgba(79, 70, 229, 0.12), transparent);
  animation: scan 8s linear infinite;
}
@keyframes scan {
  0%   { top: -2px; opacity: 0; }
  5%   { opacity: 1; }
  95%  { opacity: 1; }
  100% { top: 100%; opacity: 0; }
}

/* ─── 发光粒子 ─── */
.bg-orb {
  position: absolute;
  border-radius: 50%;
  pointer-events: none;
  filter: blur(70px);
}
.bg-orb-1 {
  width: 500px;
  height: 500px;
  top: -150px;
  left: -100px;
  background: radial-gradient(circle, rgba(79, 70, 229, 0.10) 0%, transparent 70%);
  animation: breathe 9s ease-in-out infinite;
}
.bg-orb-2 {
  width: 350px;
  height: 350px;
  bottom: -80px;
  right: -60px;
  background: radial-gradient(circle, rgba(34, 197, 94, 0.09) 0%, transparent 70%);
  animation: breathe 11s ease-in-out infinite 2.5s;
}
.bg-orb-3 {
  width: 250px;
  height: 250px;
  top: 45%;
  right: 12%;
  background: radial-gradient(circle, rgba(56, 189, 248, 0.07) 0%, transparent 70%);
  animation: breathe 7s ease-in-out infinite 1.2s;
}
@keyframes breathe {
  0%, 100% { opacity: 0.7; transform: scale(1); }
  50%       { opacity: 1;   transform: scale(1.12); }
}

/* ─── 登录卡片（白色磨砂玻璃）─── */
.login-card {
  position: relative;
  z-index: 10;
  width: 400px;
  padding: 40px 36px 32px;
  background: rgba(255, 255, 255, 0.88);
  backdrop-filter: blur(24px);
  -webkit-backdrop-filter: blur(24px);
  border: 1px solid rgba(226, 232, 240, 0.9);
  border-radius: 16px;
  box-shadow:
    0 20px 40px rgba(15, 23, 42, 0.10),
    0 4px 12px rgba(15, 23, 42, 0.06),
    0 0 0 1px rgba(255, 255, 255, 0.6) inset;
  animation: cardEnter 0.5s cubic-bezier(0.16, 1, 0.3, 1) both;
}
@keyframes cardEnter {
  from { opacity: 0; transform: translateY(24px); }
  to   { opacity: 1; transform: translateY(0); }
}

/* ─── Logo 区 ─── */
.logo-area {
  text-align: center;
  margin-bottom: 28px;
}
.logo-title {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 2px;
  margin-bottom: 8px;
  min-height: 48px;
}

/* ─── 流星动画区域 ─── */
.plane-fly-zone {
  position: absolute;
  inset: 0;
  pointer-events: none;
  overflow: visible;
  z-index: 2;
}

/* 流星尾迹：右端固定，向左延伸，右亮左渐透明 */
.meteor-trail {
  position: absolute;
  top: 50%;
  right: 0;
  height: 2px;
  width: 0;
  border-radius: 2px;
  transform: translateY(-50%);
  background: linear-gradient(
    to left,
    rgba(255, 255, 255, 0.95) 0%,
    rgba(200, 180, 255, 0.85) 12%,
    rgba(56, 189, 248, 0.70) 35%,
    rgba(34, 197, 94, 0.45) 65%,
    transparent 100%
  );
  box-shadow: 0 0 4px rgba(200, 180, 255, 0.5), 0 0 8px rgba(56, 189, 248, 0.3);
  animation: meteorTrail 1.4s ease-out 0.2s forwards;
}
@keyframes meteorTrail {
  0%   { width: 0;     opacity: 1; }
  55%  { width: 155px; opacity: 1; }
  100% { width: 165px; opacity: 0; }
}

/* 流星头：亮白圆点 + 紫/蓝多层光晕，右向左划过后上扬消失 */
.meteor-head {
  position: absolute;
  top: 50%;
  right: -6px;
  width: 10px;
  height: 10px;
  border-radius: 50%;
  background: radial-gradient(circle, #FFFFFF 0%, rgba(200, 180, 255, 0.8) 55%, transparent 100%);
  transform: translateY(-50%);
  box-shadow:
    0 0 6px 2px rgba(255, 255, 255, 0.95),
    0 0 14px 5px rgba(168, 155, 254, 0.75),
    0 0 24px 8px rgba(56, 189, 248, 0.45);
  animation: meteorHead 1.4s ease-out 0.2s forwards;
  z-index: 3;
}
@keyframes meteorHead {
  0%   { transform: translate(0,      -50%); opacity: 1; }
  55%  { transform: translate(-155px, -50%); opacity: 1; }
  80%  { transform: translate(-172px, -60%); opacity: 0.7; }
  100% { transform: translate(-195px, -75%); opacity: 0; }
}

.logo-type {
  font-family: 'Fira Code', monospace;
  font-size: 36px;
  font-weight: 700;
  letter-spacing: 2px;
  min-width: 140px;
  display: inline-block;
  position: relative;
  z-index: 1;
}
.logo-char {
  display: inline-block;
  background: linear-gradient(
    90deg,
    #FF6B6B, #FF9F43, #F9CA24,
    #22C55E, #38BDF8, #A29BFE,
    #FF6B6B
  );
  background-size: 300% 100%;
  -webkit-background-clip: text;
  background-clip: text;
  -webkit-text-fill-color: transparent;
  color: transparent;
  text-shadow: none;
  animation: charRainbow 6s linear infinite;
}
@keyframes charRainbow {
  0%   { background-position: 0%   50%; }
  100% { background-position: 300% 50%; }
}
.logo-sub {
  font-family: 'Inter', sans-serif;
  font-size: 13px;
  color: #64748B;
  margin: 0 0 16px;
  letter-spacing: 0.3px;
}
.logo-divider {
  width: 40px;
  height: 2px;
  background: linear-gradient(90deg, transparent, #22C55E, transparent);
  margin: 0 auto;
  border-radius: 1px;
}

/* ─── 表单 ─── */
.login-form {
  margin-bottom: 8px;
}
.submit-item {
  margin-top: 4px;
}

/* ─── Input 覆盖 ─── */
:deep(.tech-input .arco-input-wrapper),
:deep(.tech-input.arco-input-password) {
  background: #F8FAFC !important;
  border: 1px solid #E2E8F0 !important;
  border-radius: 8px !important;
  height: 44px !important;
  transition: border-color 200ms ease, box-shadow 200ms ease !important;
}
:deep(.tech-input .arco-input-wrapper:hover),
:deep(.tech-input.arco-input-password:hover) {
  border-color: #CBD5E1 !important;
}
:deep(.tech-input .arco-input-wrapper:focus-within),
:deep(.tech-input.arco-input-password:focus-within) {
  border-color: #22C55E !important;
  box-shadow: 0 0 0 3px rgba(34, 197, 94, 0.12) !important;
  background: #FFFFFF !important;
}
:deep(.tech-input .arco-input) {
  font-family: 'Inter', sans-serif !important;
  font-size: 14px !important;
  color: #0F172A !important;
}
:deep(.tech-input .arco-input::placeholder) {
  color: #94A3B8 !important;
}

.input-icon {
  color: #94A3B8;
  font-size: 15px;
}

/* ─── 登录按钮 ─── */
.login-btn {
  height: 46px !important;
  font-family: 'Inter', sans-serif !important;
  font-size: 15px !important;
  font-weight: 600 !important;
  letter-spacing: 1.5px !important;
  background: linear-gradient(135deg, #16a34a, #22C55E) !important;
  border: none !important;
  border-radius: 8px !important;
  color: #FFFFFF !important;
  display: flex !important;
  align-items: center !important;
  justify-content: center !important;
  gap: 8px !important;
  box-shadow: 0 3px 12px rgba(34, 197, 94, 0.30) !important;
  transition: filter 150ms ease, transform 150ms ease, box-shadow 150ms ease !important;
}
.login-btn:hover:not(:disabled) {
  filter: brightness(1.05) !important;
  box-shadow: 0 5px 18px rgba(34, 197, 94, 0.40) !important;
}
.login-btn:active:not(:disabled) {
  transform: scale(0.97) !important;
  filter: brightness(0.97) !important;
}
.btn-icon {
  font-size: 16px;
}

/* ─── 卡片底部 ─── */
.card-footer {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  margin-top: 20px;
  padding-top: 16px;
  border-top: 1px solid #E2E8F0;
}
.footer-tag {
  font-family: 'Fira Code', monospace;
  font-size: 11px;
  color: #94A3B8;
  letter-spacing: 0.5px;
}
.footer-sep {
  color: #CBD5E1;
  font-size: 12px;
}

/* ─── prefers-reduced-motion ─── */
@media (prefers-reduced-motion: reduce) {
  .bg-scan::after,
  .bg-orb,
  .meteor-head,
  .meteor-trail,
  .logo-char { animation: none !important; }
  .login-card { animation: none !important; }
  .login-btn  { transition: none !important; }
}
</style>
