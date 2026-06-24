package gateway

import (
	"errors"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PostgresStore implements Store using PostgreSQL/GORM.
// It allows multiple API replicas to share gateway registry and device bindings
// without requiring Redis.
type PostgresStore struct {
	db *gorm.DB
}

func NewPostgresStore(db *gorm.DB) Store {
	return &PostgresStore{db: db}
}

func (s *PostgresStore) RegisterGateway(info *GatewayInfo) error {
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"endpoint", "internal_url", "region", "capacity", "current_conns", "last_heartbeat", "updated_at"}),
	}).Create(&models.GatewayRegistry{
		ID:            info.ID,
		Endpoint:      info.Endpoint,
		InternalURL:   info.InternalURL,
		Region:        info.Region,
		Capacity:      info.Capacity,
		CurrentConns:  info.CurrentConns,
		LastHeartbeat: info.LastHeartbeat,
	}).Error
}

func (s *PostgresStore) HeartbeatGateway(gatewayID string, currentConns int) error {
	now := time.Now()
	res := s.db.Model(&models.GatewayRegistry{}).
		Where("id = ?", gatewayID).
		Updates(map[string]any{
			"current_conns":  currentConns,
			"last_heartbeat": now.UnixMilli(),
			"updated_at":     now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("gateway %s not found", gatewayID)
	}
	return nil
}

func (s *PostgresStore) ListGateways() ([]*GatewayInfo, error) {
	var rows []models.GatewayRegistry
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*GatewayInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, &GatewayInfo{
			ID:            r.ID,
			Endpoint:      r.Endpoint,
			InternalURL:   r.InternalURL,
			Region:        r.Region,
			Capacity:      r.Capacity,
			CurrentConns:  r.CurrentConns,
			LastHeartbeat: r.LastHeartbeat,
		})
	}
	return out, nil
}

func (s *PostgresStore) GetGateway(gatewayID string) (*GatewayInfo, error) {
	var row models.GatewayRegistry
	err := s.db.Where("id = ?", gatewayID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("gateway %s not found", gatewayID)
	}
	if err != nil {
		return nil, err
	}
	return &GatewayInfo{
		ID:            row.ID,
		Endpoint:      row.Endpoint,
		InternalURL:   row.InternalURL,
		Region:        row.Region,
		Capacity:      row.Capacity,
		CurrentConns:  row.CurrentConns,
		LastHeartbeat: row.LastHeartbeat,
	}, nil
}

func (s *PostgresStore) RemoveGateway(gatewayID string) error {
	return s.db.Delete(&models.GatewayRegistry{ID: gatewayID}).Error
}

func (s *PostgresStore) RemoveGatewayWithDevices(gatewayID string) ([]string, error) {
	var deviceIDs []string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.GatewayDeviceBinding{}).
			Where("gateway_id = ?", gatewayID).
			Pluck("device_id", &deviceIDs).Error; err != nil {
			return err
		}
		if err := tx.Where("gateway_id = ?", gatewayID).Delete(&models.GatewayDeviceBinding{}).Error; err != nil {
			return err
		}
		return tx.Delete(&models.GatewayRegistry{ID: gatewayID}).Error
	})
	return deviceIDs, err
}

func (s *PostgresStore) BindDevice(deviceID, gatewayID, connID string) (string, string, error) {
	var oldGatewayID, oldConnID string
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var existing models.GatewayDeviceBinding
		err := tx.Where("device_id = ?", deviceID).First(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			oldGatewayID = ""
			oldConnID = ""
		} else {
			oldGatewayID = existing.GatewayID
			oldConnID = existing.ConnID
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "device_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"gateway_id", "conn_id", "updated_at"}),
		}).Create(&models.GatewayDeviceBinding{
			DeviceID:  deviceID,
			GatewayID: gatewayID,
			ConnID:    connID,
		}).Error
	})
	if err != nil {
		return "", "", err
	}
	return oldGatewayID, oldConnID, nil
}

func (s *PostgresStore) UnbindDevice(deviceID string) error {
	return s.db.Delete(&models.GatewayDeviceBinding{DeviceID: deviceID}).Error
}

func (s *PostgresStore) GetDeviceGateway(deviceID string) (string, error) {
	var row models.GatewayDeviceBinding
	err := s.db.Where("device_id = ?", deviceID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", fmt.Errorf("device %s not bound", deviceID)
	}
	if err != nil {
		return "", err
	}
	return row.GatewayID, nil
}

func (s *PostgresStore) ListDevicesByGateway(gatewayID string) ([]string, error) {
	var deviceIDs []string
	err := s.db.Model(&models.GatewayDeviceBinding{}).
		Where("gateway_id = ?", gatewayID).
		Pluck("device_id", &deviceIDs).Error
	return deviceIDs, err
}

func (s *PostgresStore) TryLock(key string, ttl time.Duration) (bool, error) {
	var acquired bool
	err := s.db.Raw("SELECT pg_try_advisory_lock(hashtext(?))", key).Scan(&acquired).Error
	if err != nil {
		return false, err
	}
	return acquired, nil
}

func (s *PostgresStore) GetOrInitEpoch(initVal int64) (int64, error) {
	err := s.db.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&models.ServerEpoch{SingletonKey: "singleton", EpochValue: initVal}).Error
	if err != nil {
		return 0, err
	}
	var row models.ServerEpoch
	if err := s.db.Where("singleton_key = ?", "singleton").First(&row).Error; err != nil {
		return 0, err
	}
	return row.EpochValue, nil
}
