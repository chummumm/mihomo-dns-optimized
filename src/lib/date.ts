import dayjs from 'dayjs'
import relativeTime from 'dayjs/plugin/relativeTime'
import 'dayjs/locale/zh-cn'

import { type Lang } from '@i18n'

dayjs.extend(relativeTime)

const dateTimeFormatters = {
    en_US: new Intl.DateTimeFormat('en-US', { dateStyle: 'medium', timeStyle: 'long' }),
    zh_CN: new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'long' }),
}

export function formatDateTime (date: Date, lang: Lang): string {
    return Number.isFinite(date.getTime()) ? dateTimeFormatters[lang].format(date) : '-'
}

export function fromNow (date: Date, lang: Lang): string {
    const locale = lang === 'en_US' ? 'en' : 'zh-cn'
    return dayjs(date).locale(locale).from(dayjs())
}
