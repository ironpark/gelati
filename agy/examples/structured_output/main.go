// Command structured_output asks the agent for a typed JSON result instead
// of free text: a tool fetches raw meeting notes and the agent returns a
// MeetingSummary (upstream examples/getting_started/structured_output.py).
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/ironpark/gelati/agy"
)

// ActionItem is a single action item from a meeting.
type ActionItem struct {
	Assignee string `json:"assignee" description:"The person assigned to the action item."`
	Task     string `json:"task" description:"A description of the task to be completed."`
	Deadline string `json:"deadline" description:"When the task should be completed."`
}

// MeetingSummary is the structured output; its JSON schema is derived from
// the type.
type MeetingSummary struct {
	ActionItems []ActionItem `json:"action_items"`
}

var fetchNotes = agy.NewTool("fetch_unstructured_meeting_notes", "Retrieves the raw unstructured notes for a given meeting ID.",
	func(_ context.Context, _ *agy.ToolContext, in struct {
		MeetingID string `json:"meeting_id"`
	}) (string, error) {
		if in.MeetingID != "meeting-2026-05" {
			return "Error: Meeting notes not found.", nil
		}
		return "Discussed launch timeline for project X. Alice agreed to update the textproto tests by Monday. " +
			"Bob mentioned he will run the final E2E benchmarks tomorrow. " +
			"I will push the release build once the tests are green.", nil
	})

func main() {
	ctx := context.Background()
	agent, err := agy.NewAgent(agy.Options{
		Tools:          []*agy.Tool{fetchNotes},
		ResponseSchema: MeetingSummary{},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := agent.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer agent.Close()

	stream, err := agent.Chat(ctx, agy.Text("Use the fetch_unstructured_meeting_notes tool to retrieve notes for "+
		"'meeting-2026-05' and return the meeting summary with the appropriate action item list. "+
		"Ensure each action item includes 'assignee', 'task', and 'deadline'."))
	if err != nil {
		log.Fatal(err)
	}
	res, err := stream.Result(ctx)
	if err != nil {
		log.Fatal(err)
	}
	var summary MeetingSummary
	if err := res.DecodeStructuredOutput(&summary); err != nil {
		log.Fatalf("no structured summary (%v); final text: %s", err, res.Text())
	}
	fmt.Println("  === Structured Meeting Action Items ===")
	for _, item := range summary.ActionItems {
		fmt.Printf("  - Assignee: %s\n    Task:     %s\n    Deadline: %s\n\n", item.Assignee, item.Task, item.Deadline)
	}
}
