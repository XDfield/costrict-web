package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	redisKeyGatewayRegistry  = "gateway:registry"
	redisKeyGatewayHeartbeat = "gateway:heartbeat"
	redisKeyDeviceGateway    = "device:gateway"
)

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(client *redis.Client) Store {
	return &RedisStore{client: client}
}

func (s *RedisStore) RegisterGateway(info *GatewayInfo) error {
	ctx := context.Background()
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	pipe := s.client.Pipeline()
	pipe.HSet(ctx, redisKeyGatewayRegistry, info.ID, data)
	pipe.HSet(ctx, redisKeyGatewayHeartbeat, info.ID, now)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) HeartbeatGateway(gatewayID string, currentConns int) error {
	ctx := context.Background()
	data, err := s.client.HGet(ctx, redisKeyGatewayRegistry, gatewayID).Bytes()
	if err == redis.Nil {
		return fmt.Errorf("gateway %s not found", gatewayID)
	}
	if err != nil {
		return err
	}

	var info GatewayInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}

	now := time.Now().UnixMilli()
	info.CurrentConns = currentConns
	info.LastHeartbeat = now

	updated, err := json.Marshal(&info)
	if err != nil {
		return err
	}

	pipe := s.client.Pipeline()
	pipe.HSet(ctx, redisKeyGatewayRegistry, gatewayID, updated)
	pipe.HSet(ctx, redisKeyGatewayHeartbeat, gatewayID, now)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *RedisStore) ListGateways() ([]*GatewayInfo, error) {
	ctx := context.Background()
	entries, err := s.client.HGetAll(ctx, redisKeyGatewayRegistry).Result()
	if err != nil {
		return nil, err
	}

	heartbeats, err := s.client.HGetAll(ctx, redisKeyGatewayHeartbeat).Result()
	if err != nil {
		return nil, err
	}

	result := make([]*GatewayInfo, 0, len(entries))
	for _, v := range entries {
		var info GatewayInfo
		if err := json.Unmarshal([]byte(v), &info); err != nil {
			continue
		}
		if hbStr, ok := heartbeats[info.ID]; ok {
			var hb int64
			fmt.Sscanf(hbStr, "%d", &hb)
			info.LastHeartbeat = hb
		}
		result = append(result, &info)
	}
	return result, nil
}

func (s *RedisStore) RemoveGateway(gatewayID string) error {
	ctx := context.Background()
	pipe := s.client.Pipeline()
	pipe.HDel(ctx, redisKeyGatewayRegistry, gatewayID)
	pipe.HDel(ctx, redisKeyGatewayHeartbeat, gatewayID)
	_, err := pipe.Exec(ctx)
	return err
}

func (s *RedisStore) RemoveGatewayWithDevices(gatewayID string) ([]string, error) {
	ctx := context.Background()
	deviceIDs, err := s.ListDevicesByGateway(gatewayID)
	if err != nil {
		return nil, err
	}
	pipe := s.client.Pipeline()
	pipe.HDel(ctx, redisKeyGatewayRegistry, gatewayID)
	pipe.HDel(ctx, redisKeyGatewayHeartbeat, gatewayID)
	for _, devID := range deviceIDs {
		pipe.HDel(ctx, redisKeyDeviceGateway, devID)
	}
	_, err = pipe.Exec(ctx)
	return deviceIDs, err
}

func (s *RedisStore) BindDevice(deviceID, gatewayID string) error {
	return s.client.HSet(context.Background(), redisKeyDeviceGateway, deviceID, gatewayID).Err()
}

func (s *RedisStore) UnbindDevice(deviceID string) error {
	return s.client.HDel(context.Background(), redisKeyDeviceGateway, deviceID).Err()
}

func (s *RedisStore) GetDeviceGateway(deviceID string) (string, error) {
	gwID, err := s.client.HGet(context.Background(), redisKeyDeviceGateway, deviceID).Result()
	if err == redis.Nil {
		return "", fmt.Errorf("device %s not found", deviceID)
	}
	return gwID, err
}

func (s *RedisStore) ListDevicesByGateway(gatewayID string) ([]string, error) {
	ctx := context.Background()
	entries, err := s.client.HGetAll(ctx, redisKeyDeviceGateway).Result()
	if err != nil {
		return nil, err
	}
	var deviceIDs []string
	for devID, gwID := range entries {
		if gwID == gatewayID {
			deviceIDs = append(deviceIDs, devID)
		}
	}
	return deviceIDs, nil
}

func (s *RedisStore) TryLock(key string, ttl time.Duration) (bool, error) {
	return s.client.SetNX(context.Background(), key, "1", ttl).Result()
}

func (s *RedisStore) GetOrInitEpoch(initVal int64) (int64, error) {
	ctx := context.Background()
	const key = "server:epoch"
	set, err := s.client.SetNX(ctx, key, initVal, 0).Result()
	if err != nil {
		return 0, err
	}
	if set {
		return initVal, nil
	}
	val, err := s.client.Get(ctx, key).Int64()
	return val, err
}
