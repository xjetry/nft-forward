/* 授权文本：用户详情页“复制授权”写出、“粘贴授权”读入的那种文本。两头在
   这里成对定义：formatGrantLine 写出的每一行，parseGrantText 都要能读回同样
   的三项设置（见 grantText.test.ts）。

   每行一个节点：`节点名 | max=规则数上限 | quota=流量配额GB | rate=限速`，
   空行与 # 开头的行跳过，各项可以省略。 */

import { bytesToGBText } from './fmt.js'

/* 一条授权在文本里没写某一项时取的值。规则数上限 10 与服务端的缺省一致
   （internal/server 的 defaultGrantMaxRules）。 */
export const GRANT_DEFAULTS = Object.freeze({ maxRules: 10, quotaGB: 0, rateMBytes: 0 })

/* 每一项的写法与取值范围。数字只认十进制的规范写法：`1.2.3`、`1e3` 这类
   写法若交给 Number() 会得到 NaN 或意料之外的值，NaN 发给服务端后变成 0，
   也就是“不限”。规则数上限至少为 1：服务端把不大于 0 的值当成没写、记成默认
   值（batchApplyGrants），照 0 预览就与实际写入的不一致。 */
const SETTINGS = [
  { key: 'maxRules', pattern: /^max=(\d+)$/i, valid: v => v >= 1 },
  { key: 'quotaGB', pattern: /^quota=(\d+(?:\.\d+)?)\s*GB$/i, valid: () => true },
  { key: 'rateMBytes', pattern: /^rate=(\d+)$/i, valid: () => true },
]

/**
 * 解析授权文本。
 *
 * 读不懂的项不会被悄悄换成默认值：它们原样记在这一行的 invalid 里，由调用方
 * 决定怎么办（套用设置时必须先改对，否则写进去的不是用户写的值）。
 *
 * @param {string} text
 * @param {Array<{ id: number, name: string }>} allNodes 用来把节点名对应到节点
 * @returns {Array<{ name: string, maxRules: number, quotaGB: number, rateMBytes: number,
 *   nodeId: number | null, found: boolean, invalid: string[] }>}
 */
export function parseGrantText(text, allNodes) {
  const nodeByName = new Map(allNodes.map(n => [n.name, n]))
  return text.split('\n')
    .map(line => line.trim())
    .filter(line => line && !line.startsWith('#'))
    .map((line) => {
      const [name, ...parts] = line.split('|').map(s => s.trim())
      const row = { ...GRANT_DEFAULTS }
      const invalid = []
      for (const part of parts) {
        if (part === '') continue
        const setting = SETTINGS.find(s => s.pattern.test(part))
        const value = setting ? Number(part.match(setting.pattern)[1]) : NaN
        if (setting && setting.valid(value)) row[setting.key] = value
        else invalid.push(part)
      }
      const node = nodeByName.get(name)
      return { name, ...row, nodeId: node?.id ?? null, found: !!node, invalid }
    })
}

/**
 * 把一条授权写成一行授权文本。
 * @param {string} nodeName
 * @param {{ max_forwards: number, traffic_quota_bytes: number, rate_limit_mbytes: number }} grant
 */
export function formatGrantLine(nodeName, grant) {
  return [
    nodeName,
    `max=${grant.max_forwards}`,
    `quota=${bytesToGBText(grant.traffic_quota_bytes)}GB`,
    `rate=${grant.rate_limit_mbytes || 0}`,
  ].join(' | ')
}
