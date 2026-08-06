package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootCommand(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		wantOutput []string
	}{
		{
			name:       "help 顯示命令名與用法",
			args:       []string{"--help"},
			wantErr:    false,
			wantOutput: []string{"oryxos", "Usage:"},
		},
		{
			name:    "未知 flag 回報錯誤",
			args:    []string{"--no-such-flag"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newRootCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tt.args)

			err := cmd.Execute()

			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			for _, want := range tt.wantOutput {
				if !strings.Contains(out.String(), want) {
					t.Errorf("輸出缺少 %q，實際輸出：\n%s", want, out.String())
				}
			}
		})
	}
}
