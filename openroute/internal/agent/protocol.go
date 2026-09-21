// Package agent implements the panel side of the node protocol.
package agent

import "github.com/openroute/openroute/internal/nodeproto"

type Env = nodeproto.Env
type RegisterRequest = nodeproto.RegisterRequest
type RegisterSystem = nodeproto.RegisterSystem
type RegisterResponse = nodeproto.RegisterResponse
type HeartbeatRequest = nodeproto.HeartbeatRequest
type HeartbeatMetrics = nodeproto.HeartbeatMetrics
type RunningRule = nodeproto.RunningRule
type HeartbeatResponse = nodeproto.HeartbeatResponse
type ConfigResponse = nodeproto.ConfigResponse
type ConfigRule = nodeproto.ConfigRule
type ConfigTarget = nodeproto.ConfigTarget
type DeviceGroupConfig = nodeproto.DeviceGroupConfig
type GroupPeer = nodeproto.GroupPeer
type ReportRequest = nodeproto.ReportRequest
type RuleSyncResult = nodeproto.RuleSyncResult
type ReportStats = nodeproto.ReportStats
type RuleTrafficItem = nodeproto.RuleTrafficItem
type ReportResponse = nodeproto.ReportResponse
type TaskItem = nodeproto.TaskItem
type TasksResponse = nodeproto.TasksResponse
type TaskResultRequest = nodeproto.TaskResultRequest
type TaskResultResponse = nodeproto.TaskResultResponse
type ErrorResponse = nodeproto.ErrorResponse

const NodeTokenHeader = nodeproto.NodeTokenHeader
const NodeIDHeader = nodeproto.NodeIDHeader
const RuleStatusRunning = nodeproto.RuleStatusRunning
const RuleStatusStopped = nodeproto.RuleStatusStopped
const RuleStatusError = nodeproto.RuleStatusError
const TaskTypeUpgrade = nodeproto.TaskTypeUpgrade
const TaskTypeRestart = nodeproto.TaskTypeRestart
const TaskTypeExec = nodeproto.TaskTypeExec
const HeartbeatIntervalDefault = nodeproto.HeartbeatIntervalDefault
