import { createI18n } from 'vue-i18n'
import zh from './zh'
import en from './en'

const savedLocale = localStorage.getItem('locale') ?? 'zh'

export const i18n = createI18n({
  legacy: false,
  locale: savedLocale,
  fallbackLocale: 'zh',
  messages: { zh, en },
})

export function toggleLocale() {
  const current = i18n.global.locale.value
  const next = current === 'zh' ? 'en' : 'zh'
  i18n.global.locale.value = next
  localStorage.setItem('locale', next)
}
