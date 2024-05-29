# 背景
当前kubekey中，如果要添加命令，或修改命令，都需要提交代码并重新发版。扩展性较差。
1. 任务与框架分离（优势，目的，更方便扩展，借鉴ansible的playbook设计）
2. 支持gitops（可通过git方式，管理自动化任务）
3. 支持connector扩展
4. 支持云原生方式自动化批量任务管理

# 安装kubekey
## kubernetes中安装
```shell
helm upgrade --install --create-namespace -n kubekey-system kubekey oci://hub.kubesphere.com.cn/kubekey/kubekey
```
