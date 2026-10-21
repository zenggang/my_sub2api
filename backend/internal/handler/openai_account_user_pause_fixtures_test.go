package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type unpausedAccountUserPauseRepository struct {
	service.AccountUserPauseRepository
}

func (unpausedAccountUserPauseRepository) PausedAccountIDs(context.Context, int64) (map[int64]struct{}, error) {
	return map[int64]struct{}{}, nil
}

func (unpausedAccountUserPauseRepository) IsPaused(context.Context, int64, int64) (bool, error) {
	return false, nil
}

func newUnpausedAccountUserPauseService() *service.AccountUserPauseService {
	return service.NewAccountUserPauseService(unpausedAccountUserPauseRepository{}, nil)
}

func openAIAccountPauseTestContext() context.Context {
	return context.WithValue(context.Background(), ctxkey.UserID, int64(100))
}
