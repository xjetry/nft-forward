/* “同一时刻只执行一次”的闸门。

   用于提交类动作（保存、删除、切换）：动作在途时再次触发会被直接丢弃，
   而不是排队。判定是同步的 —— 同一个事件循环里连续两次调用，第二次一定
   看到第一次已经占用了闸门；这一点不能靠 React state 做到（state 要到
   下一次渲染才更新）。

   对“翻转”语义的接口（服务端每收到一次请求就把布尔值取反）这是正确性
   问题：放过第二次提交等于什么都没改。 */

export interface SingleFlight {
  /** 是否有动作在途。 */
  readonly busy: boolean
  /**
   * 闸门空闲时执行 fn 并返回 true（fn 抛出的错误原样抛出）；
   * 已有动作在途时不执行 fn，返回 false。
   */
  run(fn: () => Promise<unknown>): Promise<boolean>
}

export function createSingleFlight(): SingleFlight {
  let busy = false
  return {
    get busy() { return busy },
    async run(fn) {
      if (busy) return false
      busy = true
      try {
        await fn()
        return true
      } finally {
        busy = false
      }
    },
  }
}
