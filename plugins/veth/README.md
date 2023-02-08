# veth-plugin

## Why

解决缺省 CNI 为 macvlan 或 sriov 时存在的一些通讯问题。主要思路在 Pod Netns 中创建 veth-peer 设备, 使集群东西向流量以及节点与 Pod 之间的访问通过 veth-peer 完成。
集群南北向流量通过 Macvlan/Sriov 接口完成。

## How to start

1. 安装 Multus-underlay, maclan-type 选择为 macvlan-standalone.
2. 在 pod 的annotations中注入:
```shell
  annotations:
    v1.multus-cni.io/default-network: <namespace>/<name>
```

`<namespace>/<name>` 分别为对应 Multus CRD实例的名称空间和名称。

### Examples

- **veth**

```shell
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: macvlan-standalone-np0
  namespace: kube-system
spec:
  config: |-
    {
        "cniVersion": "0.3.1",
        "name": "macvlan-standalone",
        "plugins": [
            {
                "type": "macvlan",
                "master": "enp4s0f0np0",
                "mode": "bridge",
                "ipam": {
                    "type": "spiderpool",
                    "log_level": "DEBUG",
                    "log_file_path": "/var/log/spidernet/spiderpool.log",
                    "log_file_max_size": 100,
                    "log_file_max_age": 30,
                    "log_file_max_count": 10
                }
            },{
                "type": "veth",
                "service_hijack_subnet": ["172.96.0.0/18","2001:4860:fd00::/108"],
                "overlay_hijack_subnet": ["10.240.0.0/12","fd00:10:244::/96"],
                "additional_hijack_subnet":[],
                "rp_filter": {
                    "set_host": true,
                    "value": 0
                },
                "migrate_route": -1,
                "log_options": {
                  "log_level": "debug",
                  "log_file": "/var/log/meta-plugins/veth.log"
                }
            }
        ]
    }

```

- `overlay_hijack_subnet`: 缺省CNI(比如calico 或 cilium)的子网信息，包括 IPv4 和 IPv6(可选), 输入格式为 IP+掩码,如10.244.0.0/18。
- `service_hijack_subnet`: 集群 ClusterIP 的地址，包括 IPv4 和 IPv6 (可选)，输入格式为 IP+掩码,如10.244.0.0/18。
- `additional_hijack_subnet`: 额外的可自定义的路由集合，输入格式为 IP+掩码,如10.244.0.0/18
- `migrate_route`: 取值范围`-1,0,1`, 默认为 -1, 表示是否将新增网卡的默认路由移动到一个新的 route table中去。-1 表示通过网卡名自动迁移(eth0 < net1 < net2)，0 为不迁移，-1表示强制迁移。
- `skip_call`: 是否跳过调用此插件，默认为false。
- `log_options`: 日志配置。
- `rp_filter`: 设置主机 rp_filter 参数, value 取值范围为 `0,1,2`