package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/openroute/openroute/internal/api/response"
	"github.com/openroute/openroute/internal/model"
	"github.com/openroute/openroute/internal/util"
)

// NodeService 负责节点服务器的增删改查、密钥管理、批量运维与健康度评分。
//
// 节点的「实时状态」由节点侧心跳驱动（见 internal/agent），本服务只负责
// 面板侧的持久化与业务规则校验，不主动发起对节点的网络请求。
type NodeService struct {
	app *App
}

// NewNodeService 构造节点服务。
//
// 参数 a 为运行时依赖容器；返回可直接使用的 *NodeService。
func NewNodeService(a *App) *NodeService {
	return &NodeService{app: a}
}

// NodeListQuery 是节点列表的查询条件（规格书 8.6）。
type NodeListQuery struct {
	// GroupID 按节点分组筛选；0 表示不筛选。
	GroupID uint64
	// Role 按角色筛选：inbound / outbound / both；空串表示不筛选。
	Role string
	// Online 按在线状态筛选；nil 表示不筛选。
	Online *bool
	// Keyword 关键字，匹配名称、公网 IPv4、内网 IP、备注。
	Keyword string
	// Page 页码，从 1 开始。
	Page int
	// PageSize 每页条数。
	PageSize int
	// Sort 排序字段，默认 id。
	Sort string
	// Order 排序方向：asc / desc，默认 desc。
	Order string
}

// NodeCreateInput 是创建节点的入参（规格书 8.6 创建节点请求）。
type NodeCreateInput struct {
	Name     string   `json:"name"`
	Role     string   `json:"role"`
	GroupIDs []uint64 `json:"group_ids"`
	Weight   int      `json:"weight"`
	MaxConn  int      `json:"max_conn"`
	Remark   string   `json:"remark"`

	// 端口配置，允许创建时直接指定。
	DirectPort int `json:"direct_port"`
	WsPort     int `json:"ws_port"`
	TlsPort    int `json:"tls_port"`
	UdpPort    int `json:"udp_port"`
	RevPort    int `json:"rev_port"`

	// 静态连接地址（出口用）。
	ConnectHost string `json:"connect_host"`
	IsStatic    bool   `json:"is_static"`
}

// NodeUpdateInput 是更新节点的入参，字段为零值时保持原值。
//
// 使用指针区分「未传」与「显式传 0 / 空串」，避免前端局部更新把字段清空。
type NodeUpdateInput struct {
	Name        *string   `json:"name"`
	Role        *string   `json:"role"`
	GroupIDs    *[]uint64 `json:"group_ids"`
	Weight      *int      `json:"weight"`
	MaxConn     *int      `json:"max_conn"`
	Remark      *string   `json:"remark"`
	ConnectHost *string   `json:"connect_host"`
	IsStatic    *bool     `json:"is_static"`
	DirectPort  *int      `json:"direct_port"`
	WsPort      *int      `json:"ws_port"`
	TlsPort     *int      `json:"tls_port"`
	UdpPort     *int      `json:"udp_port"`
	RevPort     *int      `json:"rev_port"`
}

// NodeCreateResult 是创建节点的返回结构（规格书 8.6 创建节点响应）。
//
// 形状固定为 {id, name, token, install_command, status}，前端据此渲染
// 「复制安装命令」与「二维码」两块内容。
type NodeCreateResult struct {
	ID             uint64 `json:"id"`
	Name           string `json:"name"`
	Token          string `json:"token"`
	InstallCommand string `json:"install_command"`
	Status         string `json:"status"`
}

// List 按条件分页查询节点列表。
//
// 参数 ctx 为上下文；q 为查询条件（分组、角色、在线状态、关键字、分页）。
// 返回节点列表与总数。
func (s *NodeService) List(ctx context.Context, q NodeListQuery) ([]model.Node, int64, error) {
	db := s.app.DB.WithContext(ctx).Model(&model.Node{})

	if q.Role != "" {
		db = db.Where("role = ?", q.Role)
	}
	if q.Online != nil {
		db = db.Where("online = ?", *q.Online)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		db = db.Where("name LIKE ? OR public_ipv4 LIKE ? OR public_ipv6 LIKE ? OR private_ip LIKE ? OR remark LIKE ?",
			like, like, like, like, like)
	}

	if q.GroupID > 0 {
		// group_ids 是 JSON 数组文本，三种方言都没有统一的数组操作符，
		// 且 SQLite 的 LIKE 把 "[" 当作字符集语法（"[1" 匹配不到 "[1]"），
		// 因此这里在应用层做精确过滤后再分页，保证三种方言行为一致。
		candidates := make([]model.Node, 0)
		if err := db.Order(s.orderClause(q)).Find(&candidates).Error; err != nil {
			return nil, 0, response.Wrap(response.CodeInternal, err, "查询节点列表失败")
		}
		matched := filterNodesByGroup(candidates, q.GroupID)
		return paginateNodes(matched, q.Page, q.PageSize), int64(len(matched)), nil
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "查询节点总数失败")
	}

	items := make([]model.Node, 0)
	err := db.Order(s.orderClause(q)).
		Offset((q.Page - 1) * q.PageSize).
		Limit(q.PageSize).
		Find(&items).Error
	if err != nil {
		return nil, 0, response.Wrap(response.CodeInternal, err, "查询节点列表失败")
	}
	return items, total, nil
}

// paginateNodes 对已排序的节点切片做内存分页。
//
// 仅用于「分组筛选」这类无法在 SQL 层精确过滤的场景。
func paginateNodes(items []model.Node, page, pageSize int) []model.Node {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	start := (page - 1) * pageSize
	if start >= len(items) {
		return []model.Node{}
	}
	end := start + pageSize
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// filterNodesByGroup 在内存中精确过滤出属于指定分组的节点。
func filterNodesByGroup(items []model.Node, groupID uint64) []model.Node {
	out := make([]model.Node, 0, len(items))
	for i := range items {
		if nodeGroupHit(items[i].GroupIDs.AsUint64Slice(), groupID) {
			out = append(out, items[i])
		}
	}
	return out
}

// nodeGroupHit 判断分组 ID 是否出现在节点的分组列表中。
func nodeGroupHit(ids []uint64, target uint64) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

// orderClause 构造安全的排序子句，字段白名单化，避免 SQL 注入。
func (s *NodeService) orderClause(q NodeListQuery) string {
	allowed := map[string]bool{
		"id": true, "name": true, "created_at": true, "updated_at": true,
		"last_seen": true, "online": true, "weight": true, "health_score": true,
	}
	field := strings.TrimSpace(q.Sort)
	if !allowed[field] {
		field = "id"
	}
	dir := "DESC"
	if strings.EqualFold(q.Order, "asc") {
		dir = "ASC"
	}
	return field + " " + dir
}

// Get 按 ID 查询节点详情。
//
// 节点不存在时返回 40401。
func (s *NodeService) Get(ctx context.Context, id uint64) (*model.Node, error) {
	if id == 0 {
		return nil, response.Field(response.CodeParamInvalid, "id", id, "节点 ID 不能为空")
	}
	var n model.Node
	if err := s.app.DB.WithContext(ctx).First(&n, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, response.Field(response.CodeNotFound, "id", id, "节点不存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}
	return &n, nil
}

// Create 创建节点并生成节点密钥与安装命令。
//
// 行为（规格书 5.1 / 8.6）：
//   - 名称做 EqualFold 归一化查重，重复返回 40901；
//   - 生成唯一节点密钥（前缀 nsk_）；
//   - 返回 {id, name, token, install_command, status} 四件套。
//
// 参数 ctx 为上下文；in 为创建入参；baseURL 为面板对外地址
// （为空时回退到配置的 listen 地址），用于拼装安装命令。
// 返回创建结果或错误。
func (s *NodeService) Create(ctx context.Context, in NodeCreateInput, baseURL string) (*NodeCreateResult, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, response.Field(response.CodeParamInvalid, "name", in.Name, "节点名称不能为空")
	}
	if len([]rune(name)) > 64 {
		return nil, response.Field(response.CodeParamOutOfRange, "name", name, "节点名称不能超过 64 个字符")
	}
	role := normalizeNodeRole(in.Role)
	if role == "" {
		return nil, response.Field(response.CodeParamInvalid, "role", in.Role, "角色只能是 inbound / outbound / both")
	}
	if err := s.checkNameConflict(ctx, name, 0); err != nil {
		return nil, err
	}

	token, err := util.GenerateNodeToken()
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "生成节点密钥失败")
	}

	weight := in.Weight
	if weight <= 0 {
		weight = 1
	}

	n := model.Node{
		Name:        name,
		Token:       token,
		Role:        role,
		GroupIDs:    model.FromAny(normalizeIDList(in.GroupIDs)),
		Weight:      weight,
		MaxConn:     in.MaxConn,
		Remark:      util.Truncate(in.Remark, 255),
		DirectPort:  in.DirectPort,
		WsPort:      in.WsPort,
		TlsPort:     in.TlsPort,
		UdpPort:     in.UdpPort,
		RevPort:     in.RevPort,
		ConnectHost: strings.TrimSpace(in.ConnectHost),
		IsStatic:    in.IsStatic,
	}

	if err := s.app.DB.WithContext(ctx).Create(&n).Error; err != nil {
		if isDuplicateError(err) {
			return nil, response.New(response.CodeNameConflict, "节点名称已存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "创建节点失败")
	}

	if err := s.validateGroupsForRole(ctx, role, n.GroupIDs.AsUint64Slice()); err != nil {
		// 分组与角色冲突不阻断创建（用户可能先建节点再调分组），仅清空分组归属。
		s.app.Log.Warn("节点分组与角色不匹配，已清空分组归属",
			zap.Uint64("node_id", n.ID), zap.Error(err))
		n.GroupIDs = model.FromAny([]uint64{})
		_ = s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", n.ID).
			Update("group_ids", n.GroupIDs).Error
	}

	return &NodeCreateResult{
		ID:             n.ID,
		Name:           n.Name,
		Token:          n.Token,
		InstallCommand: s.buildInstallCommand(baseURL, n.Token),
		Status:         nodeStatusText(n.Online),
	}, nil
}

// Update 更新节点信息，未传字段保持原值。
//
// 名称变更同样走归一化查重；分组变更会校验与角色的匹配关系。
func (s *NodeService) Update(ctx context.Context, id uint64, in NodeUpdateInput) (*model.Node, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, response.Field(response.CodeParamInvalid, "name", *in.Name, "节点名称不能为空")
		}
		if !util.EqualFoldName(name, n.Name) {
			if err := s.checkNameConflict(ctx, name, id); err != nil {
				return nil, err
			}
		}
		updates["name"] = name
	}
	if in.Role != nil {
		role := normalizeNodeRole(*in.Role)
		if role == "" {
			return nil, response.Field(response.CodeParamInvalid, "role", *in.Role, "角色只能是 inbound / outbound / both")
		}
		updates["role"] = role
	}
	if in.Remark != nil {
		updates["remark"] = util.Truncate(*in.Remark, 255)
	}
	if in.Weight != nil {
		if *in.Weight < 0 {
			return nil, response.Field(response.CodeParamOutOfRange, "weight", *in.Weight, "权重不能为负数")
		}
		updates["weight"] = *in.Weight
	}
	if in.MaxConn != nil {
		if *in.MaxConn < 0 {
			return nil, response.Field(response.CodeParamOutOfRange, "max_conn", *in.MaxConn, "最大连接数不能为负数")
		}
		updates["max_conn"] = *in.MaxConn
	}
	if in.ConnectHost != nil {
		updates["connect_host"] = util.Truncate(strings.TrimSpace(*in.ConnectHost), 255)
	}
	if in.IsStatic != nil {
		updates["is_static"] = *in.IsStatic
	}
	if in.DirectPort != nil {
		updates["direct_port"] = *in.DirectPort
	}
	if in.WsPort != nil {
		updates["ws_port"] = *in.WsPort
	}
	if in.TlsPort != nil {
		updates["tls_port"] = *in.TlsPort
	}
	if in.UdpPort != nil {
		updates["udp_port"] = *in.UdpPort
	}
	if in.RevPort != nil {
		updates["rev_port"] = *in.RevPort
	}
	if in.GroupIDs != nil {
		role := n.Role
		if r, ok := updates["role"].(string); ok {
			role = r
		}
		if err := s.validateGroupsForRole(ctx, role, *in.GroupIDs); err != nil {
			return nil, err
		}
		updates["group_ids"] = model.FromAny(normalizeIDList(*in.GroupIDs))
	}

	if len(updates) == 0 {
		return n, nil
	}
	updates["updated_at"] = timeNow()

	if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		if isDuplicateError(err) {
			return nil, response.New(response.CodeNameConflict, "节点名称已存在")
		}
		return nil, response.Wrap(response.CodeInternal, err, "更新节点失败")
	}

	// 影响转发行为的字段变更需要节点重新拉取配置。
	s.app.BumpConfigVersion("node_update")
	return s.Get(ctx, id)
}

// Delete 删除节点。
//
// 行为（规格书 6.1）：删除前必须确认；被入口组/出口组引用的节点
// 返回 40903（资源正在被引用）；该节点所属入口组上的规则会被标记为
// 同步失败并进入待处理清单。
func (s *NodeService) Delete(ctx context.Context, id uint64) error {
	n, err := s.Get(ctx, id)
	if err != nil {
		return err
	}

	// 1. 被设备组引用则拒绝删除。
	//    node_ids 同样是 JSON 数组文本，在应用层做精确成员判断。
	var allGroups []model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).Find(&allGroups).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "检查节点引用失败")
	}
	referencing := make([]string, 0, 2)
	for i := range allGroups {
		if nodeGroupHit(allGroups[i].NodeIDs.AsUint64Slice(), id) {
			referencing = append(referencing, allGroups[i].Name)
		}
	}
	if len(referencing) > 0 {
		return response.New(response.CodeResourceInUse,
			fmt.Sprintf("节点 %s 仍被 %d 个设备组引用（%s），请先移出设备组",
				n.Name, len(referencing), strings.Join(referencing, "、")))
	}

	// 2. 找出该节点所属的入口设备组，这些组上的规则需要标记为待处理。
	groups, err := s.inboundGroupsOf(ctx, id)
	if err != nil {
		return err
	}
	groupIDs := make([]uint64, 0, len(groups))
	for gid := range groups {
		groupIDs = append(groupIDs, gid)
	}

	err = s.app.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 3. 受影响规则标记为同步失败并写入待处理原因（规格书 6.1）。
		if len(groupIDs) > 0 {
			if err := tx.Model(&model.ForwardRule{}).
				Where("inbound_group_id IN ?", groupIDs).
				Updates(map[string]interface{}{
					"sync_status": model.SyncFailed,
					"sync_error":  util.Truncate("节点 "+n.Name+" 已删除，请重新分配入口设备组", 512),
				}).Error; err != nil {
				return err
			}
		}
		// 4. 清理该节点遗留的任务。
		if err := tx.Where("node_id = ?", id).Delete(&model.NodeTask{}).Error; err != nil {
			return err
		}
		// 5. 删除节点自身。
		if err := tx.Where("id = ?", id).Delete(&model.Node{}).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return response.AsAppError(err)
	}

	s.app.BumpConfigVersion("node_delete")
	s.app.Log.Info("节点已删除", zap.Uint64("node_id", id), zap.String("name", n.Name))
	return nil
}

// ResetToken 重新生成节点密钥。
//
// 旧密钥立即失效，节点需要重新执行安装命令或以新密钥更新 config.yml。
// 返回节点 ID、新密钥与刷新后的安装命令。
func (s *NodeService) ResetToken(ctx context.Context, id uint64, baseURL string) (*NodeCreateResult, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	token, err := util.GenerateNodeToken()
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "生成节点密钥失败")
	}
	if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"token":      token,
			"online":     false,
			"updated_at": timeNow(),
		}).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "重置节点密钥失败")
	}

	s.app.Log.Info("节点密钥已重置", zap.Uint64("node_id", id), zap.String("name", n.Name))
	return &NodeCreateResult{
		ID:             id,
		Name:           n.Name,
		Token:          token,
		InstallCommand: s.buildInstallCommand(baseURL, token),
		Status:         nodeStatusText(false),
	}, nil
}

// GenerateInstallCommand 为已存在的节点重新生成安装命令。
//
// 参数 ctx 为上下文；id 为节点 ID；baseURL 为面板对外地址。
// 返回安装命令文本或错误。
func (s *NodeService) GenerateInstallCommand(ctx context.Context, id uint64, baseURL string) (string, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return s.buildInstallCommand(baseURL, n.Token), nil
}

// ---------------------------------------------------------------- 批量操作

// NodeBatchInput 是批量操作的公共入参。
type NodeBatchInput struct {
	// NodeIDs 目标节点 ID 列表。
	NodeIDs []uint64 `json:"node_ids"`
	// GroupID 也支持按分组批量操作；与 NodeIDs 可同时使用，取并集。
	GroupID uint64 `json:"group_id"`
}

// NodeBatchExecInput 是批量执行命令的入参。
type NodeBatchExecInput struct {
	NodeBatchInput
	// Command 要执行的 shell 命令，最长 2048 字节。
	Command string `json:"command"`
	// Timeout 命令超时秒数，0 使用默认 30 秒。
	Timeout int `json:"timeout"`
}

// NodeBatchGroupInput 是批量改分组的入参。
type NodeBatchGroupInput struct {
	NodeBatchInput
	// GroupIDs 目标分组；Mode 为 replace / add / remove。
	GroupIDs []uint64 `json:"group_ids"`
	Mode     string   `json:"mode"`
}

// NodeBatchWeightInput 是批量改权重的入参。
type NodeBatchWeightInput struct {
	NodeBatchInput
	Weight int `json:"weight"`
}

// NodeBatchDisableInput 是批量禁用/启用的入参。
type NodeBatchDisableInput struct {
	NodeBatchInput
	// Disable 为 true 表示禁用（置为离线并从负载均衡中剔除）。
	Disable bool `json:"disable"`
}

// NodeBatchResult 是批量操作的逐节点结果。
type NodeBatchResult struct {
	// Succeeded 成功的节点 ID。
	Succeeded []uint64 `json:"succeeded"`
	// Failed 失败的节点 ID 与原因。
	Failed []NodeBatchFailure `json:"failed"`
	// TaskIDs 本次创建的任务 ID（升级/重启/执行命令类操作）。
	TaskIDs []uint64 `json:"task_ids,omitempty"`
}

// NodeBatchFailure 描述单个节点失败的原因。
type NodeBatchFailure struct {
	NodeID uint64 `json:"node_id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// resolveBatchNodes 把批量入参解析为节点列表。
//
// NodeIDs 与 GroupID 可同时存在，取并集；结果按 ID 升序去重。
func (s *NodeService) resolveBatchNodes(ctx context.Context, in NodeBatchInput) ([]model.Node, error) {
	ids := normalizeIDList(in.NodeIDs)
	items := make([]model.Node, 0)

	if len(ids) > 0 {
		if err := s.app.DB.WithContext(ctx).Where("id IN ?", ids).Order("id ASC").Find(&items).Error; err != nil {
			return nil, response.Wrap(response.CodeInternal, err, "查询节点失败")
		}
	}
	if in.GroupID > 0 {
		var all []model.Node
		if err := s.app.DB.WithContext(ctx).Order("id ASC").Find(&all).Error; err != nil {
			return nil, response.Wrap(response.CodeInternal, err, "按分组查询节点失败")
		}
		byGroup := filterNodesByGroup(all, in.GroupID)
		seen := make(map[uint64]bool, len(items))
		for i := range items {
			seen[items[i].ID] = true
		}
		for i := range byGroup {
			if !seen[byGroup[i].ID] {
				items = append(items, byGroup[i])
			}
		}
	}
	if len(items) == 0 {
		return nil, response.New(response.CodeParamInvalid, "没有匹配到任何节点")
	}
	return items, nil
}

// BatchUpgrade 批量创建升级任务（规格书 8.6）。
//
// 每个节点创建一条 pending 任务并尽力即时推送；离线节点由轮询兜底。
func (s *NodeService) BatchUpgrade(ctx context.Context, in NodeBatchInput) (*NodeBatchResult, error) {
	nodes, err := s.resolveBatchNodes(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.dispatchBatch(ctx, nodes, model.TaskUpgrade, nil), nil
}

// BatchRestart 批量创建重启任务。
func (s *NodeService) BatchRestart(ctx context.Context, in NodeBatchInput) (*NodeBatchResult, error) {
	nodes, err := s.resolveBatchNodes(ctx, in)
	if err != nil {
		return nil, err
	}
	return s.dispatchBatch(ctx, nodes, model.TaskRestart, nil), nil
}

// BatchExec 批量创建执行命令任务。
//
// 命令为空返回参数错误；节点侧设置 DISABLE_EXECUTE=1 时该节点记为失败。
func (s *NodeService) BatchExec(ctx context.Context, in NodeBatchExecInput) (*NodeBatchResult, error) {
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" {
		return nil, response.Field(response.CodeParamInvalid, "command", in.Command, "命令不能为空")
	}
	if len(cmd) > 2048 {
		return nil, response.Field(response.CodeParamOutOfRange, "command", cmd, "命令长度不能超过 2048 字节")
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 30
	}
	payload := map[string]interface{}{"command": cmd, "timeout": timeout}

	nodes, err := s.resolveBatchNodes(ctx, in.NodeBatchInput)
	if err != nil {
		return nil, err
	}
	return s.dispatchBatch(ctx, nodes, model.TaskExec, payload), nil
}

// BatchGroup 批量调整节点分组。
//
// Mode 取值：replace（默认，覆盖）/ add（追加）/ remove（移除）。
func (s *NodeService) BatchGroup(ctx context.Context, in NodeBatchGroupInput) (*NodeBatchResult, error) {
	nodes, err := s.resolveBatchNodes(ctx, in.NodeBatchInput)
	if err != nil {
		return nil, err
	}
	targets := normalizeIDList(in.GroupIDs)
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "replace"
	}
	switch mode {
	case "replace", "add", "remove":
	default:
		return nil, response.Field(response.CodeParamInvalid, "mode", in.Mode, "模式只能是 replace / add / remove")
	}

	res := &NodeBatchResult{Succeeded: []uint64{}, Failed: []NodeBatchFailure{}}
	for i := range nodes {
		n := nodes[i]
		cur := n.GroupIDs.AsUint64Slice()
		next := mergeIDList(cur, targets, mode)
		if err := s.validateGroupsForRole(ctx, n.Role, next); err != nil {
			res.Failed = append(res.Failed, NodeBatchFailure{NodeID: n.ID, Name: n.Name, Reason: response.AsAppError(err).Msg})
			continue
		}
		if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", n.ID).
			Updates(map[string]interface{}{
				"group_ids":  model.FromAny(next),
				"updated_at": timeNow(),
			}).Error; err != nil {
			res.Failed = append(res.Failed, NodeBatchFailure{NodeID: n.ID, Name: n.Name, Reason: "写入失败: " + err.Error()})
			continue
		}
		res.Succeeded = append(res.Succeeded, n.ID)
	}
	s.app.BumpConfigVersion("node_batch_group")
	return res, nil
}

// BatchWeight 批量修改出口节点负载均衡权重。
//
// 权重必须是 0~100 的整数；0 表示不参与负载均衡。
func (s *NodeService) BatchWeight(ctx context.Context, in NodeBatchWeightInput) (*NodeBatchResult, error) {
	if in.Weight < 0 || in.Weight > 100 {
		return nil, response.Field(response.CodeParamOutOfRange, "weight", in.Weight, "权重必须在 0~100 之间")
	}
	nodes, err := s.resolveBatchNodes(ctx, in.NodeBatchInput)
	if err != nil {
		return nil, err
	}

	res := &NodeBatchResult{Succeeded: []uint64{}, Failed: []NodeBatchFailure{}}
	for i := range nodes {
		if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", nodes[i].ID).
			Updates(map[string]interface{}{
				"weight":     in.Weight,
				"updated_at": timeNow(),
			}).Error; err != nil {
			res.Failed = append(res.Failed, NodeBatchFailure{NodeID: nodes[i].ID, Name: nodes[i].Name, Reason: "写入失败: " + err.Error()})
			continue
		}
		res.Succeeded = append(res.Succeeded, nodes[i].ID)
	}
	s.app.BumpConfigVersion("node_batch_weight")
	return res, nil
}

// BatchDisable 批量禁用/启用节点。
//
// 禁用把节点置为离线并停止下发配置，节点侧收到后不再监听任何端口。
func (s *NodeService) BatchDisable(ctx context.Context, in NodeBatchDisableInput) (*NodeBatchResult, error) {
	nodes, err := s.resolveBatchNodes(ctx, in.NodeBatchInput)
	if err != nil {
		return nil, err
	}

	res := &NodeBatchResult{Succeeded: []uint64{}, Failed: []NodeBatchFailure{}}
	for i := range nodes {
		updates := map[string]interface{}{
			"online":     false,
			"updated_at": timeNow(),
		}
		if in.Disable {
			updates["last_error"] = "已被管理员批量禁用"
		} else {
			updates["last_error"] = ""
		}
		if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", nodes[i].ID).
			Updates(updates).Error; err != nil {
			res.Failed = append(res.Failed, NodeBatchFailure{NodeID: nodes[i].ID, Name: nodes[i].Name, Reason: "写入失败: " + err.Error()})
			continue
		}
		res.Succeeded = append(res.Succeeded, nodes[i].ID)
	}
	s.app.BumpConfigVersion("node_batch_disable")
	return res, nil
}

// dispatchBatch 为一批节点创建任务并尽力即时推送。
//
// 参数 taskType 为 upgrade / restart / exec；payload 为任务附加参数。
// 返回逐节点结果：长连在线的节点立即推送成功，其余节点创建的 pending
// 任务由节点侧轮询 GET /api/node/tasks 拉取。
func (s *NodeService) dispatchBatch(ctx context.Context, nodes []model.Node, taskType string, payload map[string]interface{}) *NodeBatchResult {
	res := &NodeBatchResult{Succeeded: []uint64{}, Failed: []NodeBatchFailure{}, TaskIDs: []uint64{}}

	for i := range nodes {
		n := nodes[i]
		t, err := s.createTask(ctx, n.ID, taskType, payload)
		if err != nil {
			res.Failed = append(res.Failed, NodeBatchFailure{NodeID: n.ID, Name: n.Name, Reason: response.AsAppError(err).Msg})
			continue
		}
		res.TaskIDs = append(res.TaskIDs, t.ID)
		res.Succeeded = append(res.Succeeded, n.ID)

		// 长连在线则即时推送；离线节点由轮询兜底，不算失败。
		s.app.Hub().BroadcastToNode(n.ID, Event{
			Type: "task_pending",
			Data: map[string]interface{}{
				"task_id": t.ID,
				"type":    taskType,
			},
		})
	}
	return res
}

// ---------------------------------------------------------------- 单节点运维

// Upgrade 创建单节点升级任务。
//
// 节点离线时返回 60001（节点离线，无法下发配置）；
// 节点设置了 DISABLE_EXECUTE=1 时返回 40304。
// 返回创建的任务与错误（任务已创建但仍返回错误时，调用方应提示「已排队」）。
func (s *NodeService) Upgrade(ctx context.Context, id uint64) (*model.NodeTask, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireExecutable(n); err != nil {
		return nil, err
	}
	t, err := s.createTask(ctx, id, model.TaskUpgrade, nil)
	if err != nil {
		return nil, err
	}
	s.app.Hub().BroadcastToNode(id, Event{
		Type: "task_pending",
		Data: map[string]interface{}{"task_id": t.ID, "type": model.TaskUpgrade},
	})
	return t, nil
}

// Restart 创建单节点重启任务。
func (s *NodeService) Restart(ctx context.Context, id uint64) (*model.NodeTask, error) {
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireOnline(n); err != nil {
		return nil, err
	}
	t, err := s.createTask(ctx, id, model.TaskRestart, nil)
	if err != nil {
		return nil, err
	}
	s.app.Hub().BroadcastToNode(id, Event{
		Type: "task_pending",
		Data: map[string]interface{}{"task_id": t.ID, "type": model.TaskRestart},
	})
	return t, nil
}

// Exec 在指定节点执行一条 shell 命令。
//
// 节点侧设置 DISABLE_EXECUTE=1 时会被拒绝（返回 40304），
// 该约定通过节点上报的 last_error 透出，避免面板盲目下发命令。
func (s *NodeService) Exec(ctx context.Context, id uint64, command string, timeout int) (*model.NodeTask, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, response.Field(response.CodeParamInvalid, "command", command, "命令不能为空")
	}
	if len(command) > 2048 {
		return nil, response.Field(response.CodeParamOutOfRange, "command", command, "命令长度不能超过 2048 字节")
	}
	n, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.requireExecutable(n); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30
	}
	payload := map[string]interface{}{"command": command, "timeout": timeout}

	t, err := s.createTask(ctx, id, model.TaskExec, payload)
	if err != nil {
		return nil, err
	}
	s.app.Hub().BroadcastToNode(id, Event{
		Type: "task_pending",
		Data: map[string]interface{}{"task_id": t.ID, "type": model.TaskExec},
	})
	return t, nil
}

// requireOnline 检查节点在线，离线时返回 60001。
func (s *NodeService) requireOnline(n *model.Node) error {
	if !n.Online {
		return response.New(response.CodeNodeOffline,
			fmt.Sprintf("节点 %s 当前离线，无法执行该操作", n.Name))
	}
	return nil
}

// requireExecutable 检查节点是否允许远程执行。
//
// 依赖两个信号：节点是否在线、节点的 last_error 是否报告了
// DISABLE_EXECUTE 约定。任何一项不满足都返回对应的业务错误。
// 之所以把 DISABLE_EXECUTE 作为错误上报而不是静默忽略，是因为
// 面板必须让运维知道「命令没有下发」而不是「已经下发但没生效」。
func (s *NodeService) requireExecutable(n *model.Node) error {
	if isExecDisabled(n.LastError) {
		return response.New(response.CodeForbidden,
			"节点已设置 DISABLE_EXECUTE=1，拒绝远程执行与升级")
	}
	return s.requireOnline(n)
}

// execDisabledMarker 是节点上报「已禁用远程执行」时使用的标记文本。
const execDisabledMarker = "DISABLE_EXECUTE"

// isExecDisabled 判断节点上报的错误文本是否表示远程执行被禁用。
func isExecDisabled(lastError string) bool {
	return strings.Contains(strings.ToUpper(lastError), execDisabledMarker)
}

// createTask 为节点创建一条 pending 任务。
func (s *NodeService) createTask(ctx context.Context, nodeID uint64, taskType string, payload map[string]interface{}) (*model.NodeTask, error) {
	t := model.NodeTask{
		NodeID: nodeID,
		Type:   taskType,
		Status: model.TaskPending,
	}
	if len(payload) > 0 {
		t.Payload = model.FromAny(payload)
	}
	if err := s.app.DB.WithContext(ctx).Create(&t).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "创建节点任务失败")
	}
	s.app.Log.Info("节点任务已创建",
		zap.Uint64("node_id", nodeID),
		zap.String("type", taskType),
		zap.Uint64("task_id", t.ID))
	return &t, nil
}

// PendingTasks 返回节点待执行的任务列表（供节点轮询）。
//
// 只返回 pending / running 状态的任务，按创建时间升序，最多 50 条。
func (s *NodeService) PendingTasks(ctx context.Context, nodeID uint64) ([]model.NodeTask, error) {
	items := make([]model.NodeTask, 0)
	err := s.app.DB.WithContext(ctx).
		Where("node_id = ? AND status IN ?", nodeID, []string{model.TaskPending, model.TaskRunning}).
		Order("id ASC").Limit(50).Find(&items).Error
	if err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询节点任务失败")
	}
	return items, nil
}

// TaskResultInput 是节点上报任务结果的载荷。
type TaskResultInput struct {
	TaskID uint64 `json:"task_id"`
	// Status 为 done / failed。
	Status string `json:"status"`
	// Result 为命令原始输出，截断到 8192 字节。
	Result string `json:"result"`
}

// ApplyTaskResult 记录节点上报的任务执行结果。
//
// 参数 ctx 为上下文；nodeID 为上报节点；in 为结果载荷。
// 返回错误。
func (s *NodeService) ApplyTaskResult(ctx context.Context, nodeID uint64, in TaskResultInput) error {
	if in.TaskID == 0 {
		return response.Field(response.CodeParamInvalid, "task_id", in.TaskID, "任务 ID 不能为空")
	}
	status := model.TaskDone
	if strings.EqualFold(strings.TrimSpace(in.Status), model.TaskFailed) {
		status = model.TaskFailed
	}

	res := s.app.DB.WithContext(ctx).Model(&model.NodeTask{}).
		Where("id = ? AND node_id = ?", in.TaskID, nodeID).
		Updates(map[string]interface{}{
			"status":     status,
			"result":     util.Truncate(in.Result, 8192),
			"updated_at": timeNow(),
		})
	if res.Error != nil {
		return response.Wrap(response.CodeInternal, res.Error, "更新任务结果失败")
	}
	if res.RowsAffected == 0 {
		return response.Field(response.CodeNotFound, "task_id", in.TaskID, "任务不存在或不属于该节点")
	}
	return nil
}

// ---------------------------------------------------------------- 健康度评分

// HealthScoreWeights 是健康度评分的权重配置（规格书 6.1）。
type HealthScoreWeights struct {
	// Online 在线率权重。
	Online float64
	// Sync 同步成功率权重。
	Sync float64
	// CPUMem 资源水位权重（CPU 与内存各占一半，取更差的一项）。
	CPUMem float64
	// Conn 连接失败率权重。
	Conn float64
}

// defaultHealthWeights 是默认权重：在线率最重要，其次是同步与资源。
var defaultHealthWeights = HealthScoreWeights{Online: 40, Sync: 30, CPUMem: 20, Conn: 10}

// UnhealthyThreshold 是健康度低于该值时前端标红。
const UnhealthyThreshold = 60

// WarningThreshold 是健康度低于该值时前端标黄。
const WarningThreshold = 80

// ComputeHealthScore 计算节点健康度评分（规格书 6.1）。
//
// 四个维度合成 0~100 分：
//   - 在线率：在线满分；离线按「最后心跳距今 / 离线阈值」线性衰减；
//   - 同步失败率：由 LastError 是否非空与是否含同步关键字估计；
//   - CPU/内存水位：取 CPU 使用率与内存使用率中较差的一项；
//   - 连接失败率：由连接数上限与错误文本估计。
//
// 参数 n 为节点；返回 0~100 的整数评分。
func (s *NodeService) ComputeHealthScore(n *model.Node) int {
	if n == nil {
		return 0
	}

	// 1. 在线率：在线满分；离线按最后心跳时间线性衰减，超过 10 倍离线阈值归零。
	onlineScore := 0.0
	if n.Online {
		onlineScore = 100
	} else if n.LastSeen != nil {
		offlineSec := float64(s.app.Config.OfflineNodeTime)
		if offlineSec <= 0 {
			offlineSec = 20
		}
		gap := time.Since(*n.LastSeen).Seconds()
		switch {
		case gap <= offlineSec:
			onlineScore = 100
		case gap >= offlineSec*10:
			onlineScore = 0
		default:
			onlineScore = 100 * (1 - (gap-offlineSec)/(offlineSec*9))
		}
	}

	// 2. 同步失败率：节点上报的同步错误与最近错误各扣一半。
	//    model.Node 没有独立的同步错误字段，LastError 同时承载两类信息：
	//    包含「同步」或 sync 关键字时视为同步失败，否则视为运行期错误。
	syncScore := 100.0
	lastErr := strings.TrimSpace(n.LastError)
	if lastErr != "" {
		syncScore -= 50
	}
	if strings.Contains(lastErr, "同步") || strings.Contains(strings.ToLower(lastErr), "sync") {
		syncScore -= 50
	}
	if syncScore < 0 {
		syncScore = 0
	}

	// 3. 资源水位：CPU 与内存使用率取更差的一项。
	resScore := 100.0
	if n.CPUUsage > 0 {
		resScore = 100 - clampFloat(n.CPUUsage, 0, 100)
	}
	if n.MemTotal > 0 {
		mem := float64(n.MemUsed) / float64(n.MemTotal) * 100
		if v := 100 - clampFloat(mem, 0, 100); v < resScore {
			resScore = v
		}
	}
	if n.DiskTotal > 0 {
		// 磁盘只做兜底惩罚：超过 95% 才明显扣分。
		disk := float64(n.DiskUsed) / float64(n.DiskTotal) * 100
		if disk > 95 {
			resScore = resScore * 0.5
		}
	}

	// 4. 连接失败率：由连接数与错误文本估计，无数据时给满分。
	connScore := 100.0
	if n.MaxConn > 0 && n.CurrentConn > n.MaxConn {
		connScore = 30
	} else if strings.Contains(n.LastError, "connection refused") || strings.Contains(n.LastError, "连接失败") {
		connScore = 20
	}

	w := defaultHealthWeights
	total := w.Online + w.Sync + w.CPUMem + w.Conn
	if total <= 0 {
		total = 100
	}
	score := (onlineScore*w.Online + syncScore*w.Sync + resScore*w.CPUMem + connScore*w.Conn) / total

	// 离线节点直接封顶 40 分，避免「刚掉线仍显示绿色」误导运维。
	if !n.Online && score > 40 {
		score = 40
	}
	return int(clampFloat(score, 0, 100) + 0.5)
}

// RefreshHealthScores 重算全部节点的健康度并落库。
//
// 由定时任务调用；返回更新后的节点数量与错误。
func (s *NodeService) RefreshHealthScores(ctx context.Context) (int, error) {
	var nodes []model.Node
	if err := s.app.DB.WithContext(ctx).Find(&nodes).Error; err != nil {
		return 0, response.Wrap(response.CodeInternal, err, "查询节点失败")
	}
	updated := 0
	for i := range nodes {
		score := s.ComputeHealthScore(&nodes[i])
		if score == nodes[i].HealthScore {
			continue
		}
		if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", nodes[i].ID).
			Update("health_score", score).Error; err != nil {
			s.app.Log.Warn("更新节点健康度失败",
				zap.Uint64("node_id", nodes[i].ID), zap.Error(err))
			continue
		}
		updated++
	}
	return updated, nil
}

// MarkOfflineStale 把超时未上报的节点标记为离线。
//
// 参数 ctx 为上下文；阈值取自配置的 offline-node-time。
// 返回被标记为离线的节点数量。
func (s *NodeService) MarkOfflineStale(ctx context.Context) (int, error) {
	seconds := s.app.Config.OfflineNodeTime
	if seconds <= 0 {
		seconds = 20
	}
	deadline := time.Now().UTC().Add(-time.Duration(seconds) * time.Second)

	var stale []model.Node
	if err := s.app.DB.WithContext(ctx).
		Where("online = ? AND (last_seen IS NULL OR last_seen < ?)", true, deadline).
		Find(&stale).Error; err != nil {
		return 0, response.Wrap(response.CodeInternal, err, "查询超时节点失败")
	}
	marked := 0
	for i := range stale {
		if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", stale[i].ID).
			Updates(map[string]interface{}{"online": false, "updated_at": timeNow()}).Error; err != nil {
			continue
		}
		marked++
		s.app.Hub().Broadcast(Event{
			Type: "node_offline",
			Data: map[string]interface{}{"node_id": stale[i].ID, "name": stale[i].Name},
		})
	}
	return marked, nil
}

// ---------------------------------------------------------------- 配置漂移

// DriftItem 描述一处「期望配置 vs 节点实际配置」的差异。
type DriftItem struct {
	// Kind 差异类别：rule_missing / rule_extra / port_mismatch / status_mismatch。
	Kind string `json:"kind"`
	// RuleID 关联规则 ID。
	RuleID uint64 `json:"rule_id,omitempty"`
	// Expected 面板期望值。
	Expected string `json:"expected"`
	// Actual 节点上报值。
	Actual string `json:"actual"`
	// Message 人类可读的说明。
	Message string `json:"message"`
}

// DriftReport 是漂移检测结果。
type DriftReport struct {
	NodeID  uint64      `json:"node_id"`
	Name    string      `json:"name"`
	Drifted bool        `json:"drifted"`
	Items   []DriftItem `json:"items"`
	// CheckedAt 本次比对时间。
	CheckedAt time.Time `json:"checked_at"`
}

// RunningRuleSummary 是节点上报的运行中规则摘要（规格书 8.16）。
type RunningRuleSummary struct {
	RuleID uint64 `json:"rule_id"`
	Port   int    `json:"port"`
	Status string `json:"status"`
	Conn   int    `json:"conn"`
}

// DetectDrift 对比面板期望配置与节点上报的实际配置（规格书 6.14）。
//
// 参数 ctx 为上下文；nodeID 为节点；actual 为节点心跳上报的运行规则摘要。
// 返回漂移报告；调用方应当把结果通过 SaveDrift 落库。
func (s *NodeService) DetectDrift(ctx context.Context, nodeID uint64, actual []RunningRuleSummary) (*DriftReport, error) {
	n, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	expected, err := s.ExpectedRules(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	report := &DriftReport{NodeID: n.ID, Name: n.Name, CheckedAt: timeNow(), Items: []DriftItem{}}

	actualByID := make(map[uint64]RunningRuleSummary, len(actual))
	for _, r := range actual {
		actualByID[r.RuleID] = r
	}

	// 1. 期望有但节点没跑的规则 → rule_missing。
	for _, er := range expected {
		ar, ok := actualByID[er.RuleID]
		if !ok {
			report.Items = append(report.Items, DriftItem{
				Kind:     "rule_missing",
				RuleID:   er.RuleID,
				Expected: fmt.Sprintf("监听 %d", er.ListenPort),
				Actual:   "未运行",
				Message:  fmt.Sprintf("规则 %s 应在端口 %d 监听，但节点未上报运行状态", er.Name, er.ListenPort),
			})
			continue
		}
		// 2. 端口不一致 → port_mismatch。
		if er.ListenPort > 0 && ar.Port > 0 && er.ListenPort != ar.Port {
			report.Items = append(report.Items, DriftItem{
				Kind:     "port_mismatch",
				RuleID:   er.RuleID,
				Expected: fmt.Sprint(er.ListenPort),
				Actual:   fmt.Sprint(ar.Port),
				Message:  fmt.Sprintf("规则 %s 期望端口 %d，实际端口 %d", er.Name, er.ListenPort, ar.Port),
			})
		}
		// 3. 运行状态异常 → status_mismatch。
		if ar.Status != "" && !strings.EqualFold(ar.Status, "running") && !strings.EqualFold(ar.Status, "normal") {
			report.Items = append(report.Items, DriftItem{
				Kind:     "status_mismatch",
				RuleID:   er.RuleID,
				Expected: "running",
				Actual:   ar.Status,
				Message:  fmt.Sprintf("规则 %s 状态为 %s，期望 running", er.Name, ar.Status),
			})
		}
		delete(actualByID, er.RuleID)
	}

	// 4. 节点多跑了的规则 → rule_extra。
	for id, ar := range actualByID {
		report.Items = append(report.Items, DriftItem{
			Kind:     "rule_extra",
			RuleID:   id,
			Expected: "已下线",
			Actual:   fmt.Sprintf("端口 %d / %s", ar.Port, ar.Status),
			Message:  fmt.Sprintf("规则 %d 已从面板移除，但节点仍在运行", id),
		})
	}

	report.Drifted = len(report.Items) > 0
	return report, nil
}

// ExpectedRule 是面板侧的「期望规则」简要结构，用于漂移比对。
type ExpectedRule struct {
	RuleID     uint64 `json:"rule_id"`
	Name       string `json:"name"`
	ListenPort int    `json:"listen_port"`
	Protocol   string `json:"protocol"`
}

// ExpectedRules 返回某节点上「面板期望运行」的规则清单。
//
// 判定条件：规则启用、其入口设备组包含该节点。
func (s *NodeService) ExpectedRules(ctx context.Context, nodeID uint64) ([]ExpectedRule, error) {
	groups, err := s.inboundGroupsOf(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	out := []ExpectedRule{}
	if len(groups) == 0 {
		return out, nil
	}
	ids := make([]uint64, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}

	var rules []model.ForwardRule
	if err := s.app.DB.WithContext(ctx).
		Where("inbound_group_id IN ? AND enable = ?", ids, true).
		Order("id ASC").Find(&rules).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询节点期望规则失败")
	}
	for i := range rules {
		out = append(out, ExpectedRule{
			RuleID:     rules[i].ID,
			Name:       rules[i].Name,
			ListenPort: rules[i].ListenPort,
			Protocol:   ruleProtocol(&rules[i], groups[rules[i].InboundGroupID]),
		})
	}
	return out, nil
}

// SaveDrift 把漂移报告写入节点行。
//
// 参数 ctx 为上下文；report 为检测结果。返回错误。
func (s *NodeService) SaveDrift(ctx context.Context, report *DriftReport) error {
	if report == nil {
		return nil
	}
	err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", report.NodeID).
		Updates(map[string]interface{}{
			"drift_detected": report.Drifted,
			"drift_detail":   model.FromAny(report.Items),
			"updated_at":     timeNow(),
		}).Error
	if err != nil {
		return response.Wrap(response.CodeInternal, err, "保存漂移检测结果失败")
	}
	return nil
}

// GetDrift 返回节点最近一次的漂移详情。
func (s *NodeService) GetDrift(ctx context.Context, nodeID uint64) (*DriftReport, error) {
	n, err := s.Get(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	report := &DriftReport{
		NodeID:    n.ID,
		Name:      n.Name,
		Drifted:   n.DriftDetected,
		Items:     []DriftItem{},
		CheckedAt: n.UpdatedAt,
	}
	if len(n.DriftDetail) > 0 {
		var items []DriftItem
		if err := jsonUnmarshal(string(n.DriftDetail), &items); err == nil && items != nil {
			report.Items = items
		}
	}
	return report, nil
}

// FixDrift 一键纠正漂移（规格书 6.14）：重新下发期望配置并清除漂移标记。
//
// 参数 ctx 为上下文；nodeID 为节点 ID。返回错误。
func (s *NodeService) FixDrift(ctx context.Context, nodeID uint64) error {
	n, err := s.Get(ctx, nodeID)
	if err != nil {
		return err
	}
	if !n.Online {
		return response.New(response.CodeNodeOffline,
			fmt.Sprintf("节点 %s 当前离线，无法纠正漂移", n.Name))
	}

	// 1. 自增配置版本，强制节点在下次心跳时拉取全量配置。
	version := s.app.BumpConfigVersion("drift_fix")

	// 2. 清除漂移标记并记录纠正动作。
	if err := s.app.DB.WithContext(ctx).Model(&model.Node{}).Where("id = ?", nodeID).
		Updates(map[string]interface{}{
			"drift_detected": false,
			"drift_detail":   model.FromAny([]DriftItem{}),
			"last_error":     "",
			"updated_at":     timeNow(),
		}).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "纠正漂移失败")
	}

	// 3. 长连在线时即时推送，否则由节点轮询兜底。
	s.app.Hub().BroadcastToNode(nodeID, Event{
		Type: "config_changed",
		Data: map[string]interface{}{
			"config_version": version,
			"reason":         "drift_fix",
			"full":           true,
		},
	})
	return nil
}

// ---------------------------------------------------------------- 内部工具

// checkNameConflict 检查节点名称是否与已有节点重复（归一化比较，规格书 4.1）。
//
// 参数 name 为待检查名称；excludeID 为需要排除的节点 ID（更新场景传自身）。
// 重复时返回 40901。
func (s *NodeService) checkNameConflict(ctx context.Context, name string, excludeID uint64) error {
	var names []string
	q := s.app.DB.WithContext(ctx).Model(&model.Node{}).Select("name")
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	if err := q.Find(&names).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "检查节点名称失败")
	}
	for _, existing := range names {
		if util.EqualFoldName(existing, name) {
			return response.New(response.CodeNameConflict,
				fmt.Sprintf("节点名称 %q 已存在（名称比较不区分大小写）", name))
		}
	}
	return nil
}

// inboundGroupsOf 返回该节点所属的全部入口设备组，按组 ID 索引。
func (s *NodeService) inboundGroupsOf(ctx context.Context, nodeID uint64) (map[uint64]*model.DeviceGroup, error) {
	var groups []model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).
		Where("type = ?", model.GroupTypeInbound).Find(&groups).Error; err != nil {
		return nil, response.Wrap(response.CodeInternal, err, "查询入口设备组失败")
	}
	out := make(map[uint64]*model.DeviceGroup)
	for i := range groups {
		if nodeGroupHit(groups[i].NodeIDs.AsUint64Slice(), nodeID) {
			out[groups[i].ID] = &groups[i]
		}
	}
	return out, nil
}

// validateGroupsForRole 校验节点分组与节点角色的匹配性（规格书 4.2.4 / 8.4）。
//
// 规则：角色为 inbound 的节点不能出现在出口组，outbound 反之。
// 不匹配时返回 42206。
func (s *NodeService) validateGroupsForRole(ctx context.Context, role string, groupIDs []uint64) error {
	if len(groupIDs) == 0 {
		return nil
	}
	var groups []model.DeviceGroup
	if err := s.app.DB.WithContext(ctx).Where("id IN ?", groupIDs).Find(&groups).Error; err != nil {
		return response.Wrap(response.CodeInternal, err, "校验节点分组失败")
	}
	for i := range groups {
		g := &groups[i]
		if g.IsInbound() && role == model.RoleOutbound {
			return response.New(response.CodeGroupRoleMismatch,
				fmt.Sprintf("节点角色为出口，不能加入入口组 %s", g.Name))
		}
		if g.IsOutbound() && role == model.RoleInbound {
			return response.New(response.CodeGroupRoleMismatch,
				fmt.Sprintf("节点角色为入口，不能加入出口组 %s", g.Name))
		}
	}
	return nil
}

// buildInstallCommand 生成一键对接的安装命令（规格书 5.1）。
//
// 参数 baseURL 为面板对外地址（可带协议头）；token 为节点密钥。
// 返回形如 bash <(curl -fsSL http://host:port/install.sh) -u ... -t ... 的命令。
func (s *NodeService) buildInstallCommand(baseURL, token string) string {
	base := s.PanelBaseURL(baseURL)
	return fmt.Sprintf("bash <(curl -fsSL %s/install.sh) -u %s -t %s", base, base, token)
}

// PanelBaseURL 返回面板对外地址。
//
// 优先使用请求中推导出的地址（baseURL）；为空时回退到配置的 listen，
// 并把 0.0.0.0 替换为 127.0.0.1，避免生成不可达的安装命令。
func (s *NodeService) PanelBaseURL(baseURL string) string {
	if strings.TrimSpace(baseURL) != "" {
		return normalizeBaseURL(baseURL)
	}
	listen := strings.TrimSpace(s.app.Config.Listen)
	if listen == "" {
		listen = "127.0.0.1:18888"
	}
	host, port := splitHostPort(listen)
	if host == "0.0.0.0" || host == "::" || host == "" {
		host = "127.0.0.1"
	}
	return "http://" + joinHostPort(host, port)
}

// normalizeBaseURL 去掉末尾斜杠并补齐协议头。
func normalizeBaseURL(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimRight(u, "/")
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "http://" + u
	}
	return u
}

// splitHostPort 拆分 host:port，缺失端口时返回默认 18888。
func splitHostPort(s string) (string, string) {
	idx := strings.LastIndex(s, ":")
	if idx < 0 {
		return s, "18888"
	}
	host := s[:idx]
	port := s[idx+1:]
	if port == "" {
		port = "18888"
	}
	return strings.Trim(host, "[]"), port
}

// joinHostPort 拼接 host 与 port，IPv6 地址自动加方括号。
func joinHostPort(host, port string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// nodeStatusText 把在线状态转成规格书 8.6 使用的文本。
func nodeStatusText(online bool) string {
	if online {
		return "online"
	}
	return "offline"
}

// normalizeNodeRole 归一化节点角色，非法值返回空串。
func normalizeNodeRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case model.RoleInbound:
		return model.RoleInbound
	case model.RoleOutbound:
		return model.RoleOutbound
	case model.RoleBoth, "":
		return model.RoleBoth
	}
	return ""
}

// normalizeIDList 去重并移除 0，保持首次出现的顺序。
func normalizeIDList(ids []uint64) []uint64 {
	out := make([]uint64, 0, len(ids))
	seen := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// mergeIDList 按模式合并两份 ID 列表。
func mergeIDList(cur, targets []uint64, mode string) []uint64 {
	switch mode {
	case "add":
		return normalizeIDList(append(append([]uint64{}, cur...), targets...))
	case "remove":
		drop := make(map[uint64]bool, len(targets))
		for _, t := range targets {
			drop[t] = true
		}
		out := make([]uint64, 0, len(cur))
		for _, c := range cur {
			if !drop[c] {
				out = append(out, c)
			}
		}
		return out
	default: // replace
		return normalizeIDList(targets)
	}
}

// ruleProtocol 解析规则与入口组共同决定的隧道协议。
//
// 取值：direct（入口直出，无出口组）/ ws / http / tls（来自入口组配置）。
func ruleProtocol(r *model.ForwardRule, g *model.DeviceGroup) string {
	if r.OutboundGroupID == 0 {
		return "direct"
	}
	if g == nil {
		return "tls"
	}
	var cfg struct {
		Protocol string `json:"protocol"`
	}
	if len(g.Config) > 0 {
		_ = jsonUnmarshal(string(g.Config), &cfg)
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Protocol)) {
	case "ws", "http", "tls", "direct":
		return strings.ToLower(strings.TrimSpace(cfg.Protocol))
	}
	return "tls"
}

// clampFloat 把浮点数限制在 [lo, hi] 区间内。
func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// isDuplicateError 判断数据库错误是否为「唯一约束冲突」。
//
// 三种方言的错误文本各不相同，这里做宽松匹配；命中后由调用方
// 转换成 40901（名称冲突）而不是 50001。
func isDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate entry") ||
		strings.Contains(msg, "unique index") ||
		strings.Contains(msg, "duplicated key") ||
		strings.Contains(msg, "constraint failed")
}
