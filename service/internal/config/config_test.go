package config

import "testing"

// TestParseRemoteUPSNameInjection 验证 UPS 名称白名单能拒绝命令注入类输入。
// NUT 客户端会把 name 拼入 "LIST VAR <name>" 命令，若 name 允许换行/空白即可注入
// 额外的 NUT 协议指令（如 SHUTDOWN），此处须在解析层拦截。
func TestParseRemoteUPSNameInjection(t *testing.T) {
	valid := []string{
		"ups@192.168.1.50",
		"myups@host",
		"my_ups.1@10.0.0.1:3493",
	}
	for _, s := range valid {
		if _, err := ParseRemoteUPS(s); err != nil {
			t.Errorf("合法输入 %q 被拒绝: %v", s, err)
		}
	}

	invalid := []string{
		"foo\nSHUTDOWN@host", // 换行注入 NUT 命令
		"foo bar@host",       // 空白
		"foo@bar@host",       // 多余 @ 分隔
		"foo/../bar@host",    // 路径字符
		"foo;rm@host",        // 分号注入
		"foo\x00@host",       // NUL 字节
	}
	for _, s := range invalid {
		if _, err := ParseRemoteUPS(s); err == nil {
			t.Errorf("恶意输入 %q 未被拒绝", s)
		}
	}
}

// TestRedactSecretsPreserve 验证密码脱敏与回填往返一致。
func TestRedactSecretsPreserve(t *testing.T) {
	c := Config{NUTUser: "monuser", NUTPass: "trim-secret"}
	redacted := c.RedactSecrets()
	if redacted.NUTPass == "trim-secret" {
		t.Fatal("RedactSecrets 未脱敏密码")
	}
	if redacted.NUTPass != redactedPassword {
		t.Fatalf("脱敏占位符不正确: %q", redacted.NUTPass)
	}

	// 回填：占位符应沿用旧密码
	redacted.PreservePassword(c)
	if redacted.NUTPass != "trim-secret" {
		t.Fatalf("PreservePassword 未回填密码: %q", redacted.NUTPass)
	}
}

// TestMountTargetBusID 验证 bus-id 白名单：拒绝选项注入与非法格式。
// busid 会以 `--busid <id>` 交给 root 执行的 usbip，必须以 '-' 之外的字符开头。
func TestMountTargetBusID(t *testing.T) {
	valid := []string{"1-1", "1-1.2", "2-1.3.4", "127.0.0.1:3240@1-1"}
	for _, s := range valid {
		if _, err := ParseMountTarget(s, DefaultServerPort); err != nil {
			t.Errorf("合法输入 %q 被拒绝: %v", s, err)
		}
	}

	invalid := []string{
		"127.0.0.1:3240@--help",  // 选项注入
		"127.0.0.1:3240@-x",      // 以 '-' 开头
		"127.0.0.1:3240@1-1;rm",  // shell 元字符（本就无 shell，仍应拒绝）
		"127.0.0.1:3240@../../x", // 路径字符
		"127.0.0.1:3240@1-1\n2",  // 换行
	}
	for _, s := range invalid {
		if _, err := ParseMountTarget(s, DefaultServerPort); err == nil {
			t.Errorf("恶意输入 %q 未被拒绝", s)
		}
	}
}

// TestMountTargetValidatePublicHost 验证挂载目标主机被限制在回环/私网，
// 防止 root 级 usbip attach 指向公网（BadUSB）。
func TestMountTargetValidatePublicHost(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "192.168.1.10", "10.0.0.5", "172.16.0.1", "169.254.1.1"} {
		tgt, err := ParseMountTarget(host+":3240@1-1", DefaultServerPort)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", host, err)
		}
		if err := tgt.Validate(); err != nil {
			t.Errorf("回环/私网主机 %q 不应被拒绝: %v", host, err)
		}
	}
	for _, host := range []string{"8.8.8.8", "203.0.113.9", "example.com"} {
		tgt, err := ParseMountTarget(host+":3240@1-1", DefaultServerPort)
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", host, err)
		}
		if err := tgt.Validate(); err == nil {
			t.Errorf("公网/域名主机 %q 应被拒绝", host)
		}
	}
}

// TestValidateCredentialRedactedPlaceholder 验证脱敏占位符能通过校验：
// GET /api/config 返回 "******"，调用方原样回传时必须被当作"未修改"而非非法字符。
func TestValidateCredentialRedactedPlaceholder(t *testing.T) {
	c := Config{
		RemoteUPS:  "myups@127.0.0.1",
		ServerPort: DefaultServerPort,
		ServerBind: DefaultServerBind,
		HTTPPort:   DefaultHTTPPort,
		HTTPBind:   DefaultHTTPBind,
		NUTUser:    "monuser",
		NUTPass:    redactedPassword,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("脱敏占位符应通过校验，否则'读回原样提交'一律 400: %v", err)
	}

	// 真实非法字符仍必须被拒绝（不能因为放宽占位符而放开其它字符）
	c.NUTPass = "bad pass"
	if err := c.Validate(); err == nil {
		t.Fatal("含空格的密码未被拒绝")
	}
	c.NUTPass = "bad\npass"
	if err := c.Validate(); err == nil {
		t.Fatal("含换行的密码未被拒绝")
	}
}

// TestEnsureAPIToken 验证 API token 的生成、幂等与脱敏不下发。
func TestEnsureAPIToken(t *testing.T) {
	c := Config{}
	generated, err := c.EnsureAPIToken()
	if err != nil {
		t.Fatalf("首次 EnsureAPIToken 不应报错: %v", err)
	}
	if !generated {
		t.Fatal("首次 EnsureAPIToken 应生成 token")
	}
	if c.APIToken == "" {
		t.Fatal("生成的 token 为空")
	}
	if len(c.APIToken) != 64 { // 32 字节 hex = 64 字符
		t.Fatalf("token 长度异常: %d", len(c.APIToken))
	}

	// 幂等：已有 token 时不再变化
	before := c.APIToken
	generated, err = c.EnsureAPIToken()
	if err != nil {
		t.Fatalf("重复 EnsureAPIToken 不应报错: %v", err)
	}
	if generated {
		t.Fatal("已有 token 时 EnsureAPIToken 不应再生成")
	}
	if c.APIToken != before {
		t.Fatal("EnsureAPIToken 覆盖了已有 token")
	}

	// 脱敏：api_token 绝不下发
	redacted := c.RedactSecrets()
	if redacted.APIToken != "" {
		t.Fatalf("RedactSecrets 未清空 api_token: %q", redacted.APIToken)
	}
	if c.APIToken != before {
		t.Fatal("RedactSecrets 修改了原配置的 token")
	}
}
