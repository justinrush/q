package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/justinrush/q/internal/paths"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/memory"
	"github.com/justinrush/q/internal/mission"
	"github.com/spf13/cobra"
)

func buildMemorySubcommand() *cobra.Command {
	var asJSON bool
	root := &cobra.Command{Use: "memory", Short: "Save and query shared mission memory"}
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "Print JSON")

	root.AddCommand(memorySaveCommand(&asJSON), memorySearchCommand(&asJSON), memoryShowCommand(&asJSON), memoryUpdateCommand(&asJSON), memoryDeleteCommand(&asJSON), memoryStatsCommand(&asJSON))
	return root
}

func readMemoryInput(cmd *cobra.Command, path string, out any) error {
	reader := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		reader = f
	}
	b, err := io.ReadAll(io.LimitReader(reader, memory.MaxBatch+1))
	if err != nil {
		return err
	}
	if len(b) > memory.MaxBatch {
		return fmt.Errorf("memory input exceeds 128 KiB")
	}
	return json.Unmarshal(b, out)
}

func printMemory(cmd *cobra.Command, r memory.Record) error {
	if r.Deleted {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s\n", r.ID)
		return err
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  [%s] %s\nSource: %s (%s), %s, %s; %s\n%s\n\n", r.ID, r.Kind, r.Title, r.Source.MissionName, r.Source.MissionID, r.Source.OperationName, r.Source.Agent, r.UpdatedAt.Format("2006-01-02"), r.Body)
	return err
}

func memorySaveCommand(asJSON *bool) *cobra.Command {
	var file, source, capture string
	save := &cobra.Command{Use: "save", Short: "Save a JSON batch of summary and lesson records", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var batch struct {
			Records []memory.Input `json:"records"`
		}
		if err := readMemoryInput(cmd, file, &batch); err != nil {
			return err
		}
		id := source
		if id == "" {
			id = os.Getenv("Q_MISSION_ID")
		}
		if id == "" {
			return fmt.Errorf("--mission or Q_MISSION_ID is required")
		}
		c, err := connectDaemon(cmd.Context())
		if err != nil {
			return err
		}
		out, err := c.SaveMemory(cmd.Context(), api.SaveMemoryRequest{Mission: mission.MissionID(id), Capture: capture, Records: batch.Records})
		if err != nil {
			return err
		}
		return memoryOutput(cmd, out, *asJSON)
	}}
	save.Flags().StringVar(&file, "file", "-", "JSON file, or - for stdin: {\"records\":[{\"kind\":\"summary\",\"title\":\"…\",\"body\":\"…\",\"tags\":[]}]}")
	save.Flags().StringVar(&source, "mission", "", "Source mission (default Q_MISSION_ID)")
	save.Flags().StringVar(&capture, "capture", "", "Finish capture token supplied by q")
	return save
}

func memorySearchCommand(asJSON *bool) *cobra.Command {
	var q memory.Query
	search := &cobra.Command{Use: "search [keywords]", Short: "Search dated, agent-written context", RunE: func(cmd *cobra.Command, args []string) error {
		q.Keywords = strings.Join(args, " ")
		q.CallerMission = os.Getenv("Q_MISSION_ID")
		c, err := connectDaemon(cmd.Context())
		if err != nil {
			return err
		}
		out, err := c.SearchMemory(cmd.Context(), q)
		if err != nil {
			return err
		}
		return memoryOutput(cmd, out, *asJSON)
	}}
	search.Flags().StringVar(&q.Repo, "repo", "", "Repository name or normalized origin URL")
	search.Flags().StringVar(&q.Mission, "mission", "", "Source mission ID or exact name")
	search.Flags().StringVar(&q.Operation, "operation", "", "Source operation ID or exact name")
	search.Flags().StringVar(&q.Kind, "kind", "", "summary or lesson")
	search.Flags().IntVar(&q.Limit, "limit", 10, "Maximum results (up to 50)")
	return search
}

func memoryShowCommand(asJSON *bool) *cobra.Command {
	show := &cobra.Command{Use: "show <id>", Short: "Read a memory with its source", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := connectDaemon(cmd.Context())
		if err != nil {
			return err
		}
		out, err := c.ShowMemory(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return memoryOutput(cmd, out, *asJSON)
	}}
	return show
}

func memoryUpdateCommand(asJSON *bool) *cobra.Command {
	var updateFile string
	update := &cobra.Command{Use: "update <id>", Short: "Replace a memory's kind, title, body, and tags", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var in memory.Input
		if err := readMemoryInput(cmd, updateFile, &in); err != nil {
			return err
		}
		c, err := connectDaemon(cmd.Context())
		if err != nil {
			return err
		}
		out, err := c.UpdateMemory(cmd.Context(), args[0], in)
		if err != nil {
			return err
		}
		return memoryOutput(cmd, out, *asJSON)
	}}
	update.Flags().StringVar(&updateFile, "file", "-", "JSON object file, or - for stdin")
	return update
}

func memoryDeleteCommand(asJSON *bool) *cobra.Command {
	remove := &cobra.Command{Use: "delete <id>", Short: "Delete a memory on both paired hosts", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := connectDaemon(cmd.Context())
		if err != nil {
			return err
		}
		out, err := c.DeleteMemory(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return memoryOutput(cmd, out, *asJSON)
	}}
	return remove
}

func memoryOutput(cmd *cobra.Command, v any, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
	}
	switch records := v.(type) {
	case []memory.Record:
		for _, r := range records {
			if err := printMemory(cmd, r); err != nil {
				return err
			}
		}
	case memory.Record:
		return printMemory(cmd, records)
	case []memory.Result:
		for _, r := range records {
			r.Body = r.Snippet
			if err := printMemory(cmd, r.Record); err != nil {
				return err
			}
		}
		if len(records) == 0 {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "No memories found.")
			return err
		}
	}
	return nil
}

func memoryStatsCommand(asJSON *bool) *cobra.Command {
	return &cobra.Command{Use: "stats", Short: "Count memory searches by day and agent", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dirs, err := paths.Resolve(pathOverrides())
		if err != nil {
			return err
		}
		path := filepath.Join(dirs.Data, "memory-queries.jsonl")
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			if *asJSON {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "[]")
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "No memory searches recorded.\nHistory: %s\n", path)
			}
			return err
		}
		if err != nil {
			return err
		}
		defer f.Close()
		type row struct {
			Day   string `json:"day"`
			Tool  string `json:"tool"`
			Count int    `json:"count"`
		}
		counts := map[string]*row{}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			var event struct {
				At   string `json:"at"`
				Tool string `json:"tool"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
				return err
			}
			if len(event.At) < 10 {
				continue
			}
			day := event.At[:10]
			key := day + "\x00" + event.Tool
			if counts[key] == nil {
				counts[key] = &row{Day: day, Tool: event.Tool}
			}
			counts[key].Count++
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		rows := make([]row, 0, len(counts))
		for _, v := range counts {
			rows = append(rows, *v)
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Day != rows[j].Day {
				return rows[i].Day < rows[j].Day
			}
			return rows[i].Tool < rows[j].Tool
		})
		if *asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
		}
		for _, r := range rows {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s  %-12s %d\n", r.Day, r.Tool, r.Count); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "History: %s\n", path)
		return err
	}}
}
