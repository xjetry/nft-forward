/**
 * 把若干 class 片段拼成一个字符串，跳过 false / null / undefined / ''。
 * @param {...(string | false | null | undefined)} parts
 * @returns {string}
 */
export function cx(...parts) {
  return parts.filter(Boolean).join(' ')
}
