package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/HUAHUAI23/RuiQi/server/config"
	cornjob "github.com/HUAHUAI23/RuiQi/server/service/cornjob/haproxy"
	"github.com/HUAHUAI23/RuiQi/server/service/daemon"
	"github.com/haproxytech/client-native/v6/models"
	"github.com/rs/zerolog"
)

// 定义错误
var (
	ErrRunnerNotRunning     = errors.New("运行器未在运行")
	ErrRunnerAlreadyRunning = errors.New("运行器已在运行")
)

// RunnerService 运行器服务接口
type RunnerService interface {
	// 获取运行器状态
	GetStatus(ctx context.Context) (daemon.ServiceState, error)

	// 运行器操作
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	ForceStop(ctx context.Context) error
	Reload(ctx context.Context) error
	// get haproxy stats
	GetStats() (models.NativeStats, error)
}

// RunnerServiceImpl 运行器服务实现
type RunnerServiceImpl struct {
	logger zerolog.Logger
	runner daemon.ServiceRunner
}

// NewRunnerService 创建运行器服务
func NewRunnerService() (RunnerService, error) {
	logger := config.GetServiceLogger("runner")

	// 获取ServiceRunner服务
	runner, err := daemon.GetRunnerService()
	if err != nil {
		logger.Error().Err(err).Msg("获取ServiceRunner失败")
		return nil, fmt.Errorf("初始化运行器服务失败: %w", err)
	}

	return &RunnerServiceImpl{
		logger: logger,
		runner: runner,
	}, nil
}

// GetStatus 获取运行器状态
func (s *RunnerServiceImpl) GetStatus(ctx context.Context) (daemon.ServiceState, error) {
	return s.runner.GetState(), nil
}

// Start 启动运行器
func (s *RunnerServiceImpl) Start(ctx context.Context) error {
	// 检查当前状态
	if s.runner.GetState() == daemon.ServiceRunning {
		return ErrRunnerAlreadyRunning
	}

	// 更新 haproxy 服务打点数据列表
	targetList, err := cornjob.GetLatestTargetList()
	if err != nil {
		return fmt.Errorf("failed to get target list: %w", err)
	}

	cronJobService, err := cornjob.GetInstance(s.runner, targetList)
	if err != nil {
		return fmt.Errorf("failed to create cron job service: %w", err)
	}
	cronJobService.UpdateTargetList(targetList)

	// 启动服务
	err = s.runner.StartServices()
	if err != nil {
		s.logger.Error().Err(err).Msg("启动运行器失败")
		return fmt.Errorf("启动运行器失败: %w", err)
	}

	return nil
}

// Stop 停止运行器
func (s *RunnerServiceImpl) Stop(ctx context.Context) error {
	// 检查当前状态
	if s.runner.GetState() != daemon.ServiceRunning {
		return ErrRunnerNotRunning
	}

	// 停止服务
	err := s.runner.StopServices()
	if err != nil {
		s.logger.Error().Err(err).Msg("停止运行器失败")
		return fmt.Errorf("停止运行器失败: %w", err)
	}

	return nil
}

// Restart 重启运行器
func (s *RunnerServiceImpl) Restart(ctx context.Context) error {
	// 更新 haproxy 服务打点数据列表
	targetList, err := cornjob.GetLatestTargetList()
	if err != nil {
		return fmt.Errorf("failed to get target list: %w", err)
	}

	cronJobService, err := cornjob.GetInstance(s.runner, targetList)
	if err != nil {
		return fmt.Errorf("failed to create cron job service: %w", err)
	}
	cronJobService.UpdateTargetList(targetList)

	// 重启服务
	err = s.runner.Restart()
	if err != nil {
		s.logger.Error().Err(err).Msg("重启运行器失败")
		return fmt.Errorf("重启运行器失败: %w", err)
	}

	return nil
}

// ForceStop 强制停止运行器
func (s *RunnerServiceImpl) ForceStop(ctx context.Context) error {
	// 强制停止服务
	s.runner.ForceStop()
	return nil
}

// Reload 热重载运行器
func (s *RunnerServiceImpl) Reload(ctx context.Context) error {
	// 检查当前状态
	if s.runner.GetState() != daemon.ServiceRunning {
		return ErrRunnerNotRunning
	}

	// 更新 haproxy 服务打点数据列表
	targetList, err := cornjob.GetLatestTargetList()
	if err != nil {
		return fmt.Errorf("failed to get target list: %w", err)
	}

	cronJobService, err := cornjob.GetInstance(s.runner, targetList)
	if err != nil {
		return fmt.Errorf("failed to create cron job service: %w", err)
	}
	cronJobService.UpdateTargetList(targetList)

	// 热重载服务
	err = s.runner.HotReload()
	if err != nil {
		s.logger.Error().Err(err).Msg("热重载运行器失败")
		return fmt.Errorf("热重载运行器失败: %w", err)
	}

	return nil
}

func (s *RunnerServiceImpl) GetStats() (models.NativeStats, error) {
	return s.runner.GetStats()
}
