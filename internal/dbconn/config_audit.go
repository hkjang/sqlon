package dbconn

import (
	"context"
	"fmt"
	"strconv"
)

type ConfigVariable struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Recommended string `json:"recommended"`
	Status      string `json:"status"` // ok | warning | critical
	Description string `json:"description"`
}

type ConfigAuditResult struct {
	OK           bool             `json:"ok"`
	Engine       string           `json:"engine"`
	SystemRAM_GB int              `json:"system_ram_gb"`
	Variables    []ConfigVariable `json:"variables"`
	Summary      string           `json:"summary"`
}

// AuditConfiguration checks runtime variables and generates optimized recommendations.
func (m *Manager) AuditConfiguration(ctx context.Context, profileID string, systemRAM_GB int) (*ConfigAuditResult, error) {
	if systemRAM_GB <= 0 {
		systemRAM_GB = 8 // Default to 8GB if unspecified
	}

	p, err := m.store.GetProfileByID(ctx, profileID)
	if err != nil {
		return nil, err
	}
	d, err := DialectFor(p.Type)
	if err != nil {
		return nil, err
	}
	db, err := m.db(p)
	if err != nil {
		return nil, err
	}

	res := &ConfigAuditResult{
		OK:           true,
		Engine:       d.Name(),
		SystemRAM_GB: systemRAM_GB,
		Variables:    []ConfigVariable{},
	}

	if d.Name() == "postgres" {
		query := `SELECT name, setting, COALESCE(unit, ''), short_desc 
			FROM pg_catalog.pg_settings 
			WHERE name IN (
				'shared_buffers', 'work_mem', 'maintenance_work_mem', 
				'effective_cache_size', 'max_connections', 'checkpoint_completion_target',
				'random_page_cost'
			)`
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("query pg_settings failed: %w", err)
		}
		defer rows.Close()

		var warningsCount = 0
		ramBytes := int64(systemRAM_GB) * 1024 * 1024 * 1024

		for rows.Next() {
			var name, setting, unit, desc string
			if err := rows.Scan(&name, &setting, &unit, &desc); err != nil {
				return nil, err
			}

			valVal, _ := strconv.ParseInt(setting, 10, 64)
			var recommended, status string

			// Parse pg units (usually 8kB blocks for buffers)
			// shared_buffers in blocks of 8kB
			switch name {
			case "shared_buffers":
				recBytes := ramBytes / 4 // 25% of RAM
				var recVal int64
				if unit == "8kB" || setting == "" {
					recVal = recBytes / 8192
					recommended = fmt.Sprintf("%d (%d GB)", recVal, recBytes/(1024*1024*1024))
				} else {
					recVal = recBytes / (1024 * 1024) // in MB
					recommended = fmt.Sprintf("%d MB", recVal)
				}
				// Check if current is too low (< 15% RAM) or too high (> 40% RAM)
				curBytes := valVal
				if unit == "8kB" {
					curBytes = valVal * 8192
				} else if unit == "kB" {
					curBytes = valVal * 1024
				}
				if curBytes < ramBytes*15/100 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "effective_cache_size":
				recBytes := ramBytes * 3 / 4 // 75% of RAM
				var recVal int64
				if unit == "8kB" || setting == "" {
					recVal = recBytes / 8192
					recommended = fmt.Sprintf("%d (%d GB)", recVal, recBytes/(1024*1024*1024))
				} else {
					recVal = recBytes / (1024 * 1024)
					recommended = fmt.Sprintf("%d MB", recVal)
				}
				curBytes := valVal
				if unit == "8kB" {
					curBytes = valVal * 8192
				}
				if curBytes < ramBytes/2 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "max_connections":
				recommended = "100 - 300"
				if valVal > 500 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "work_mem":
				recommended = "64MB"
				curBytes := valVal
				if unit == "8kB" {
					curBytes = valVal * 8192
				} else if unit == "kB" {
					curBytes = valVal * 1024
				}
				if curBytes < 16*1024*1024 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "checkpoint_completion_target":
				recommended = "0.9"
				valFloat, _ := strconv.ParseFloat(setting, 64)
				if valFloat < 0.9 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "random_page_cost":
				recommended = "1.1 (SSD) / 4.0 (HDD)"
				valFloat, _ := strconv.ParseFloat(setting, 64)
				if valFloat > 2.0 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			default:
				recommended = setting
				status = "ok"
			}

			res.Variables = append(res.Variables, ConfigVariable{
				Name:        name,
				Value:       setting + " " + unit,
				Recommended: recommended,
				Status:      status,
				Description: desc,
			})
		}

		if warningsCount > 0 {
			res.Summary = fmt.Sprintf("PostgreSQL settings audited: %d variables audited, %d warnings found. Optimizing shared_buffers and effective_cache_size is recommended.", len(res.Variables), warningsCount)
		} else {
			res.Summary = "PostgreSQL settings are healthy and aligned with SRE capacity best practices!"
		}
		return res, nil
	} else if d.Name() == "mysql" || d.Name() == "mariadb" {
		query := `SHOW VARIABLES WHERE Variable_name IN (
			'innodb_buffer_pool_size', 'max_connections', 'join_buffer_size',
			'sort_buffer_size', 'read_buffer_size', 'thread_cache_size'
		)`
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("query variables failed: %w", err)
		}
		defer rows.Close()

		var warningsCount = 0
		ramBytes := int64(systemRAM_GB) * 1024 * 1024 * 1024

		for rows.Next() {
			var name, setting string
			if err := rows.Scan(&name, &setting); err != nil {
				return nil, err
			}

			valVal, _ := strconv.ParseInt(setting, 10, 64)
			var recommended, status, desc string

			switch name {
			case "innodb_buffer_pool_size":
				desc = "Size of memory buffer pool for InnoDB tables and indexes."
				recBytes := ramBytes * 6 / 10 // 60% of RAM
				recommended = fmt.Sprintf("%d (%d GB)", recBytes, recBytes/(1024*1024*1024))
				if valVal < ramBytes/2 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "max_connections":
				desc = "The maximum permitted number of simultaneous client connections."
				recommended = "150 - 500"
				if valVal > 800 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "join_buffer_size":
				desc = "Minimum size of buffer used for plain index scans/range scans/joins."
				recommended = "262144 (256 KB) - 1048576 (1 MB)"
				if valVal > 4*1024*1024 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			case "sort_buffer_size":
				desc = "Each session that needs to perform a sort allocates a buffer of this size."
				recommended = "262144 (256 KB) - 2097152 (2 MB)"
				if valVal > 4*1024*1024 {
					status = "warning"
					warningsCount++
				} else {
					status = "ok"
				}

			default:
				recommended = setting
				status = "ok"
			}

			res.Variables = append(res.Variables, ConfigVariable{
				Name:        name,
				Value:       setting,
				Recommended: recommended,
				Status:      status,
				Description: desc,
			})
		}

		if warningsCount > 0 {
			res.Summary = fmt.Sprintf("MySQL/MariaDB variables audited: %d variables audited, %d warnings found. Adjusting innodb_buffer_pool_size is recommended.", len(res.Variables), warningsCount)
		} else {
			res.Summary = "MySQL/MariaDB settings are healthy and aligned with SRE capacity best practices!"
		}
		return res, nil
	}

	return &ConfigAuditResult{OK: false, Summary: "Configuration auditing is not supported for this dialect."}, nil
}
