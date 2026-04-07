# Usage / ES 对接说明

## 当前接入方式

服务端已改为支持通过环境变量切换 usage provider：

- `sqlite`：沿用本地 SQLite
- `es`：直接对接 ES 指标服务

## 环境变量

```env
USAGE_PROVIDER=sqlite
USAGE_SQLITE_PATH=./data/usage/usage.db

USAGE_ES_REPORT_BASE_URL=
USAGE_ES_QUERY_BASE_URL=
USAGE_ES_REPORT_PATH=/internal/indicator/api/v1/session_turn_metrics
USAGE_ES_QUERY_PATH=/costrict_session_turn_metrics/_search
USAGE_ES_TIMEOUT_SECONDS=15
USAGE_ES_BASIC_USER=
USAGE_ES_BASIC_PASS=
USAGE_ES_INSECURE_SKIP_VERIFY=false
```

## 说明

- 当 `USAGE_PROVIDER=sqlite` 时，使用 SQLiteUsageProvider
- 当 `USAGE_PROVIDER=es` 时，使用 ESUsageProvider
- 当 `USAGE_PROVIDER=es` 时，`USAGE_ES_REPORT_BASE_URL` 与 `USAGE_ES_QUERY_BASE_URL` 必填

## ES 上报接口

服务端会将 usage report 转发到：

`PUT {USAGE_ES_REPORT_BASE_URL}{USAGE_ES_REPORT_PATH}`

请求体格式：

```json
{
  "request_id": "turn-001",
  "message_id": "msg-001",
  "occurred_at": "2026-04-01T10:20:30Z",
  "token_metrics": {
    "prompt_tokens": 100,
    "completion_tokens": 50,
    "reasoning_tokens": 0,
    "cache_read_tokens": 0,
    "cache_write_tokens": 0
  },
  "cost_metrics": {
    "cost": 0.12
  },
  "user_metrics": {
    "git_repo": "https://github.com/org/repo",
    "session_id": "sess-abc",
    "device_id": "device-001"
  },
  "label": {
    "model": "GLM-4.7",
    "provider": "openai"
  }
}
```

上报请求透传当前用户 JWT：

```http
Authorization: Bearer <user-jwt>
```

查询请求使用 Basic Auth：

```http
Authorization: Basic <base64(username:password)>
```

## ES 查询接口

服务端会查询：

`POST {USAGE_ES_QUERY_BASE_URL}{USAGE_ES_QUERY_PATH}`

请求体为裸 ES aggregation search DSL。

固定查询字段：

- repo 字段：`user_metrics.git_repo.keyword`
- user 字段：`user_id.keyword`
- 时间字段：`occurred_at`
- prompt tokens：`token_metrics.prompt_tokens`
- completion tokens：`token_metrics.completion_tokens`
- reasoning tokens：`token_metrics.reasoning_tokens`
- cache read tokens：`token_metrics.cache_read_tokens`
- cache write tokens：`token_metrics.cache_write_tokens`
- cost：`cost_metrics.cost`

请求体示例：

```json
{
  "size": 0,
  "query": {
    "bool": {
      "filter": [
        {
          "range": {
            "occurred_at": {
              "gte": "2026-03-25T00:00:00Z",
              "lte": "2026-04-02T23:59:59Z"
            }
          }
        },
        {
          "terms": {
            "user_metrics.git_repo.keyword": [
              "https://github.com/org/repo1"
            ]
          }
        },
        {
          "terms": {
            "user_id.keyword": ["user-1", "user-2"]
          }
        }
      ]
    }
  },
  "aggs": {
    "repos": {
      "terms": {
        "field": "user_metrics.git_repo.keyword",
        "size": 1000
      },
      "aggs": {
        "users": {
          "terms": {
            "field": "user_id.keyword",
            "size": 1000
          },
          "aggs": {
            "days": {
              "date_histogram": {
                "field": "occurred_at",
                "calendar_interval": "day",
                "format": "2006-01-02"
              },
              "aggs": {
                "prompt_tokens": { "sum": { "field": "token_metrics.prompt_tokens" } },
                "completion_tokens": { "sum": { "field": "token_metrics.completion_tokens" } },
                "reasoning_tokens": { "sum": { "field": "token_metrics.reasoning_tokens" } },
                "cache_read_tokens": { "sum": { "field": "token_metrics.cache_read_tokens" } },
                "cache_write_tokens": { "sum": { "field": "token_metrics.cache_write_tokens" } },
                "cost": { "sum": { "field": "cost_metrics.cost" } }
              }
            }
          }
        }
      }
    }
  }
}
```

服务端当前按 ES aggregation 响应解析：

```json
{
  "aggregations": {
    "repos": {
      "buckets": [
        {
          "key": "https://github.com/org/repo",
          "users": {
            "buckets": [
              {
                "key": "user-1",
                "days": {
                  "buckets": [
                    {
                      "key_as_string": "2026-03-25",
                      "doc_count": 3,
                      "prompt_tokens": { "value": 100 },
                      "completion_tokens": { "value": 50 },
                      "reasoning_tokens": { "value": 0 },
                      "cache_read_tokens": { "value": 0 },
                      "cache_write_tokens": { "value": 0 },
                      "cost": { "value": 0.12 }
                    }
                  ]
                }
              }
            ]
          }
        }
      ]
    }
  }
}
```

## 当前 provider 适配范围

- `GET /api/usage/activity`
- 项目 repo activity 聚合查询
- repository candidate 聚合查询

服务端会基于 ES aggregation 返回的 `repo -> user -> day` 结构在应用层完成二次聚合。
