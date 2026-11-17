/* 节点升级在界面上用到的两处判断。升级状态与“版本是否落后”都由服务端给出
   （internal/server/upgrade_status.go），这里只把它们译成界面要说的话。 */

/**
 * 服务端对“最近一次升级推送”的结论，取值与含义见 upgrade_status.go 的
 * upgradeView：
 * - `none`：没有推送记录。
 * - `pending`：推送已被节点确认，节点上报的版本还没到，仍在宽限期内。
 * - `ok`：推送已被确认，节点上报的版本已不低于推送的版本。
 * - `stuck`：推送已被确认，过了宽限期，节点上报的版本仍然更低。
 * - `error`：推送本身失败。
 * - `superseded`：记录里的那次推送没有把节点带到它的版本，但节点已不低于
 *   面板版本（通过别的途径升上来的），这条记录没有可说的了。
 */
export type UpgradeStatus = 'none' | 'pending' | 'ok' | 'stuck' | 'error' | 'superseded'

export type UpgradeVerdict = 'running' | 'succeeded' | 'failed' | 'stuck'

/**
 * 等待一次升级的结论时，服务端的状态对应哪一种结论。
 *
 * 成功只认 `ok`。`none` 与 `superseded` 的意思是“没有可展示的记录”，不是
 * “升级成功了”，所以与 `pending` 一样算还没有结论；读不懂的取值也一样。
 */
export function upgradeVerdict(status: UpgradeStatus | undefined): UpgradeVerdict {
  switch (status) {
    case 'ok':
      return 'succeeded'
    case 'error':
      return 'failed'
    case 'stuck':
      return 'stuck'
    default:
      return 'running'
  }
}

/** “升级状态”一行是否有内容可展示。 */
export function upgradeWorthShowing(status: UpgradeStatus | undefined): boolean {
  return status === 'pending' || status === 'ok' || status === 'stuck' || status === 'error'
}

export interface UpgradeAllReply {
  /** 推送已被节点确认的个数。 */
  readonly upgraded: number
  /** 是升级对象、但此刻没有连接可推的个数。 */
  readonly offline: number
  /** 推送出了错的个数。 */
  readonly failed: number
}

/**
 * 批量升级之后要告诉管理员的话。三个计数互不重叠，合起来就是这一批的全部
 * 对象（已启用、版本低于面板的节点）；有节点没推到时 `incomplete` 为 true。
 */
export function upgradeAllReport(reply: UpgradeAllReply): { readonly text: string; readonly incomplete: boolean } {
  const { upgraded, offline, failed } = reply
  if (upgraded + offline + failed === 0) {
    return { text: '没有需要升级的节点：已启用的节点里没有 Agent 版本低于面板的', incomplete: false }
  }
  if (offline + failed === 0) {
    return { text: `升级已推送到 ${upgraded} 个节点，等待它们重启`, incomplete: false }
  }
  const missed = [
    offline > 0 ? `${offline} 个不在线、没有推送` : '',
    failed > 0 ? `${failed} 个推送失败` : '',
  ].filter(Boolean).join('，')
  return { text: `已推送到 ${upgraded} 个节点；${missed}（原因见各节点的升级状态）`, incomplete: true }
}
