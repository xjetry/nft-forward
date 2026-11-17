/* 节点可以占的位置：能不能做规则的入口、能不能做中转层。界面上叫“用途”。

   位值与服务端一致（internal/chain 的 SeatEntry、SeatVia；库里是 nodes.seats 与
   user_nodes.seats 两列）。接口里这个字段的键是 roles，所以页面读的是 node.roles、
   grant.roles，写的是 { roles }。 */
export const SEAT_ENTRY = 1
export const SEAT_VIA = 2

/* 一条授权对节点用途的覆盖可选的值。0 不是“没有用途”，而是不覆盖：这个用户在这个
   节点上沿用节点自己的用途。其余的值只对这个用户生效，整体取代节点自己的用途。 */
export const GRANT_SEAT_OPTIONS = Object.freeze([
  { value: 0, label: '跟随节点' },
  { value: SEAT_ENTRY, label: '仅入口' },
  { value: SEAT_VIA, label: '仅中转' },
  { value: SEAT_ENTRY | SEAT_VIA, label: '入口 + 中转' },
])
