//go:build ignore

// make_nyanpass_sample 生成一份「Nyanpass 风格」的样例源库，
// 供迁移功能的端到端演练与人工验收使用（规格书 7.2 的输入）。
//
// 用法：
//
//	go run scripts/make_nyanpass_sample.go ./nyanpass-sample.db
//
// 生成的内容包含用户/分组/节点/设备组/规则/流量，且刻意植入两类冲突：
//   - 名称大小写冲突（Admin / admin、HK-01 / hk-01）
//   - 同入口组内的端口冲突
//
// 同时包含 Nyanpass 特有的授权与充值字段，用于验证迁移会正确丢弃它们。
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/glebarez/go-sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: go run scripts/make_nyanpass_sample.go <输出路径>")
		os.Exit(1)
	}
	path := os.Args[1]
	_ = os.Remove(path)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		fail("打开数据库失败", err)
	}
	defer db.Close()

	stmts := []string{
		// schema_version：迁移工具据此识别版本
		`CREATE TABLE schema_version (id INTEGER PRIMARY KEY, version INTEGER, applied_at TEXT)`,
		`INSERT INTO schema_version (id, version, applied_at) VALUES (1, 20260301, '2026-03-01T00:00:00Z')`,

		// 用户：含授权 / 充值等应被丢弃的字段
		`CREATE TABLE users (
			id INTEGER PRIMARY KEY, username TEXT, password TEXT, nickname TEXT,
			role TEXT, status INTEGER, group_id INTEGER, token TEXT,
			traffic_used INTEGER, traffic_limit INTEGER, speed_limit INTEGER,
			ip_limit INTEGER, conn_limit INTEGER, expire_at TEXT, remark TEXT,
			license_key TEXT, recharge_total INTEGER, expire_license_at TEXT,
			created_at TEXT, updated_at TEXT)`,
		`INSERT INTO users VALUES
			(1,'admin','$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy','管理员','admin',1,1,'tok_admin',1073741824,0,0,0,0,NULL,'',  'LIC-AAAA-BBBB',5000,'2027-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(2,'Admin','$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy','重名用户','user',1,1,'tok_admin2',0,10737418240,1048576,3,100,'2027-01-01T00:00:00Z','','LIC-CCCC-DDDD',0,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(3,'user1','5f4dcc3b5aa765d61d8327deb882cf99','普通用户','user',1,1,'tok_user1',536870912,0,0,0,0,NULL,'','','0','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		`CREATE TABLE user_groups (
			id INTEGER PRIMARY KEY, name TEXT, traffic_limit INTEGER, speed_limit INTEGER,
			ip_limit INTEGER, conn_limit INTEGER, rule_group_ids TEXT, remark TEXT,
			created_at TEXT, updated_at TEXT)`,
		`INSERT INTO user_groups VALUES (1,'默认分组',0,0,0,0,'[]','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		`CREATE TABLE nodes (
			id INTEGER PRIMARY KEY, name TEXT, token TEXT, role TEXT,
			public_ipv4 TEXT, public_ipv6 TEXT, private_ip TEXT, connect_host TEXT,
			is_static INTEGER, direct_port INTEGER, ws_port INTEGER, tls_port INTEGER,
			udp_port INTEGER, rev_port INTEGER, group_ids TEXT, online INTEGER,
			last_seen TEXT, weight INTEGER, max_conn INTEGER, os TEXT, arch TEXT,
			kernel_ver TEXT, client_ver TEXT, cpu_model TEXT, cpu_cores INTEGER,
			mem_total INTEGER, disk_total INTEGER, boot_time TEXT, remark TEXT,
			created_at TEXT, updated_at TEXT)`,
		`INSERT INTO nodes VALUES
			(1,'HK-01','old_token_1','both','203.0.113.1','','10.0.0.1','',0,10000,10001,10002,10003,10004,'[1]',1,'2026-01-01T00:00:00Z',1,0,'Debian 12','amd64','6.1.0','nc20260301','Xeon',4,8589934592,107374182400,'2026-01-01T00:00:00Z','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(2,'hk-01','old_token_2','both','203.0.113.2','','10.0.0.2','',0,10010,10011,10012,10013,10014,'[1]',1,'2026-01-01T00:00:00Z',1,0,'Debian 12','amd64','6.1.0','nc20260301','Xeon',4,8589934592,107374182400,'2026-01-01T00:00:00Z','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(3,'JP-01','old_token_3','outbound','203.0.113.3','','10.0.0.3','jp.example.com',1,10020,10021,10022,10023,10024,'[2]',1,'2026-01-01T00:00:00Z',1,0,'Ubuntu 22.04','amd64','5.15.0','nc20260301','EPYC',8,17179869184,214748364800,'2026-01-01T00:00:00Z','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		`CREATE TABLE node_groups (
			id INTEGER PRIMARY KEY, name TEXT, remark TEXT,
			created_at TEXT, updated_at TEXT)`,
		`INSERT INTO node_groups VALUES
			(1,'香港','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(2,'日本','','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		`CREATE TABLE device_groups (
			id INTEGER PRIMARY KEY, name TEXT, type TEXT, node_ids TEXT, config TEXT,
			balance TEXT, health_check_enable INTEGER, health_check_interval INTEGER,
			health_check_timeout INTEGER, health_check_fail_count INTEGER,
			health_check_succ_count INTEGER, failover_group_id INTEGER, remark TEXT,
			created_at TEXT, updated_at TEXT)`,
		// 入口组：config 结构必须与规格书 4.3 完全一致，迁移时原样搬运
		`INSERT INTO device_groups VALUES
			(1,'HK-In','inbound','[1,2]','{"allowed_host":[],"blocked_host":[],"blocked_path":[],"blocked_protocol":[],"tls_inbound_policy":0,"tls_reject_empty_sni":false,"disable_udp":false,"udp_over_tcp":false,"ipv6_group":[],"max_fail":3,"fail_timout_sec":30,"reverse_group":[],"protocol":"tls","tls":{}}','least_conn',1,10,3,3,2,0,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(2,'JP-Out','outbound','[3]','{"connect_type":"static","connect_address":"jp.example.com","connect_port":2333,"protocol":"ws","ws":{"host":"cdn.example.com","path":"/ws"},"udp_over_tcp":false}','least_conn',1,10,3,3,2,0,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		`CREATE TABLE rule_groups (
			id INTEGER PRIMARY KEY, name TEXT, sort INTEGER, remark TEXT,
			created_at TEXT, updated_at TEXT)`,
		`INSERT INTO rule_groups VALUES (1,'默认规则组',0,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		`CREATE TABLE forward_rules (
			id INTEGER PRIMARY KEY, name TEXT, user_id INTEGER, rule_group_id INTEGER,
			inbound_group_id INTEGER, listen_port INTEGER, listen_port_end INTEGER,
			outbound_group_id INTEGER, target_host TEXT, target_port TEXT,
			target_json TEXT, inbound_multiplier REAL, outbound_multiplier REAL,
			speed_limit INTEGER, conn_limit INTEGER, ip_limit INTEGER, config TEXT,
			chain_groups TEXT, reverse_enable INTEGER, reverse_port INTEGER,
			reverse_group_id INTEGER, is_sub_rule INTEGER, parent_id INTEGER, sni TEXT,
			sync_status TEXT, traffic_in INTEGER, traffic_out INTEGER, enable INTEGER,
			remark TEXT, created_at TEXT, updated_at TEXT)`,
		// 第 1、2 条：同入口组、同端口，触发端口冲突检测
		`INSERT INTO forward_rules VALUES
			(1,'rule-hk-jp-01',1,1,1,8443,0,2,'1.2.3.4','443','[{"host":"1.2.3.4","port":443,"weight":1}]',1.5,0.5,0,0,0,'{"tls":{"sni":"some.host.com","chfp":"chrome"}}','[]',0,0,0,0,0,'','normal',1073741824,536870912,1,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(2,'test-a',1,1,1,8443,0,0,'5.6.7.8','443','[{"host":"5.6.7.8","port":443,"weight":1}]',1,1,0,0,0,'{}','[]',0,0,0,0,0,'','normal',0,0,1,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(3,'test-b',1,1,1,8443,0,0,'9.9.9.9','8080','[{"host":"9.9.9.9","port":8080,"weight":2}]',1,1,0,0,0,'{}','[]',0,0,0,0,0,'','unsynced',0,0,1,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z'),
			(4,'rule-with-bad-ref',1,1,999,9000,0,0,'1.1.1.1','53','[{"host":"1.1.1.1","port":53,"weight":1}]',1,1,0,0,0,'{}','[]',0,0,0,0,0,'','unsynced',0,0,1,'引用不存在的入口组','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,

		// 流量记录：迁移时按 (date,hour,user_id,rule_id,node_id,direction) 聚合
		`CREATE TABLE traffic_logs (
			id INTEGER PRIMARY KEY, date TEXT, hour INTEGER, user_id INTEGER,
			rule_id INTEGER, node_id INTEGER, direction TEXT, raw_bytes INTEGER,
			bytes INTEGER, created_at TEXT)`,
		`INSERT INTO traffic_logs VALUES
			(1,'2026-01-01',10,1,1,1,'in',1073741824,1610612736,'2026-01-01T10:00:00Z'),
			(2,'2026-01-01',10,1,1,1,'in',536870912,805306368,'2026-01-01T10:30:00Z'),
			(3,'2026-01-01',10,1,1,1,'out',268435456,134217728,'2026-01-01T10:30:00Z'),
			(4,'2026-01-02',11,2,2,1,'in',104857600,104857600,'2026-01-02T11:00:00Z')`,

		// 支付/订单相关表：迁移时整表跳过
		`CREATE TABLE orders (id INTEGER PRIMARY KEY, user_id INTEGER, amount REAL)`,
		`INSERT INTO orders VALUES (1,1,99.9)`,
		`CREATE TABLE products (id INTEGER PRIMARY KEY, name TEXT, price REAL)`,
		`INSERT INTO products VALUES (1,'月付套餐',9.9)`,
		`CREATE TABLE payment_configs (id INTEGER PRIMARY KEY, gateway TEXT)`,
		`INSERT INTO payment_configs VALUES (1,'epay')`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			fail("执行建表/插入失败: "+s[:min(60, len(s))], err)
		}
	}

	fmt.Printf("已生成 Nyanpass 样例源库: %s\n", path)
	fmt.Println("  用户 3 / 节点 3 / 设备组 2 / 规则 4 / 流量 4")
	fmt.Println("  已植入冲突：名称大小写（Admin|admin, HK-01|hk-01）、同入口组端口 8443 冲突、规则 4 引用不存在的组 999")
	fmt.Println("  已植入应丢弃内容：orders / products / payment_configs 表，users.license_key 等字段")
}

func fail(msg string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", msg, err)
	os.Exit(1)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
