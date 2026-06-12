package ipc

const SocketPath = "/tmp/gpm.sock"

type Action string

const (
	ActionStart    Action = "start"
	ActionStop     Action = "stop"
	ActionRestart  Action = "restart"
	ActionReload   Action = "reload"
	ActionScale    Action = "scale"
	ActionDelete   Action = "delete"
	ActionList     Action = "list"
	ActionLogs     Action = "logs"
	ActionSave     Action = "save"
	ActionResurrect Action = "resurrect"
	ActionPing     Action = "ping"
)

type Request struct {
	Action  Action            `json:"action"`
	Name    string            `json:"name,omitempty"`
	Binary  string            `json:"binary,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	WorkDir string            `json:"work_dir,omitempty"`
	LogLines int             `json:"log_lines,omitempty"`

	// Service / cluster config (set by `start`)
	Instances       int    `json:"instances,omitempty"`
	Host            string `json:"host,omitempty"`
	Port            int    `json:"port,omitempty"`
	Mode            string `json:"mode,omitempty"`
	HealthPath      string `json:"health_path,omitempty"`
	ShutdownTimeout int    `json:"shutdown_timeout,omitempty"` // seconds
	PortEnv         string `json:"port_env,omitempty"`
	Watch           bool   `json:"watch,omitempty"`
	WatchInterval   int    `json:"watch_interval,omitempty"` // seconds

	// scale target (set by `scale`)
	Replicas int `json:"replicas,omitempty"`
}

type Response struct {
	OK      bool        `json:"ok"`
	Error   string      `json:"error,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}
