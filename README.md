## 参数说明

```
  -code int
        HTTP/HTTPS 响应状态码 (default 200)
  -colo string
        数据中心: 筛选数据中心例如 HKG,SJC,LAX (多个数据中心用逗号隔开,留空则忽略匹配)
  -delay int
        有效延迟（毫秒）: 超过此延迟将断开连接 (default 300)
  -domain string
        响应状态码检查的域名地址 (default "cloudflaremirrors.com/debian")
  -ipnum int
        有效IP数: 提取的有效IP数量 (default 20)
  -ips string
        转发IP类型: 指定生成IPv4还是IPv6地址 (4或6) (default "4")
  -num int
        负载IP数: 目标负载 IP 数量 (default 10)
  -port int
        转发目标端口 (default 443)
  -probe int
        池内探测间隔（秒） (default 60)
  -proben int
        每轮池内探测对每个 IP 的握手次数，用于估算丢包率 (default 3)
  -loss int
        丢包率阈值（百分比）: 探测丢包超过此值视为质量不足，
        当前 IP 连续 2 轮超阈值且池内存在显著更低丢包（≤当前一半且 ≤10%）的候选时切换 (default 20)
  -random
        是否随机生成IP，如果为false，则从CIDR中拆分出所有IP (default true)
  -tls
        是否为 TLS 端口 (default true)
  -task int
        最大并发请求数: 并发请求最大协程数 (default 100)
```

## 致谢

[股神](https://t.me/CF_NAT/38840)
