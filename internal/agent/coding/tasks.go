package coding

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Task represents a single item on the agent's internal todo list.
type Task struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"` // "todo", "in_progress", "completed", "failed"
}

// TaskList holds the thread-safe list of internal tasks/todos for the agent.
type TaskList struct {
	mu     sync.Mutex
	tasks  []Task
	nextID int
}

// NewTaskList creates and initializes a new TaskList.
func NewTaskList() *TaskList {
	return &TaskList{
		tasks:  make([]Task, 0),
		nextID: 1,
	}
}

// Add appends a new task to the list and returns it.
func (l *TaskList) Add(description string) Task {
	l.mu.Lock()
	defer l.mu.Unlock()

	task := Task{
		ID:          strconv.Itoa(l.nextID),
		Description: description,
		Status:      "todo",
	}
	l.nextID++
	l.tasks = append(l.tasks, task)
	return task
}

// Update updates an existing task's status and/or description and returns the updated task.
func (l *TaskList) Update(id string, status string, description *string) (Task, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for i, t := range l.tasks {
		if t.ID == id {
			if status != "" {
				l.tasks[i].Status = status
			}
			if description != nil {
				l.tasks[i].Description = *description
			}
			return l.tasks[i], nil
		}
	}
	return Task{}, fmt.Errorf("task with ID %s not found", id)
}

// List returns a copied slice of the internal tasks to avoid race conditions.
func (l *TaskList) List() []Task {
	l.mu.Lock()
	defer l.mu.Unlock()

	copied := make([]Task, len(l.tasks))
	copy(copied, l.tasks)
	return copied
}

func formatTaskList(tasks []Task) string {
	if len(tasks) == 0 {
		return "Internal task list is empty."
	}
	var sb strings.Builder
	sb.WriteString("Internal Task List:\n")
	for _, t := range tasks {
		sb.WriteString(fmt.Sprintf("- [%s] ID %s: %s\n", t.Status, t.ID, t.Description))
	}
	return sb.String()
}

// add_task tool

type AddTaskInput struct {
	Description string `json:"description" jsonschema:"description=The description of the task to add to your internal todo list. This is completely internal and invisible to the user."`
}

type AddTaskOutput struct {
	Message string
}

type addTaskTool struct {
	toolkit *CodingToolkit
}

func (t *addTaskTool) Name() string { return "add_task" }
func (t *addTaskTool) Description() string {
	return "Add a new task to your internal task list. This tool is strictly internal, and the user cannot see it or its results. Use it to plan, track, and manage your progress through complex jobs."
}
func (t *addTaskTool) Schema() *AddTaskInput { return &AddTaskInput{} }
func (t *addTaskTool) Execute(ctx context.Context, input *AddTaskInput) (*AddTaskOutput, error) {
	if input.Description == "" {
		return nil, fmt.Errorf("description cannot be empty")
	}
	task := t.toolkit.tasks.Add(input.Description)
	msg := fmt.Sprintf("Added task ID %s: %q.\n\n%s", task.ID, task.Description, formatTaskList(t.toolkit.tasks.List()))
	return &AddTaskOutput{Message: msg}, nil
}
func (t *addTaskTool) ToString(o *AddTaskOutput) string {
	return o.Message
}

// update_task tool

type UpdateTaskInput struct {
	ID          string  `json:"id" jsonschema:"description=The ID of the task to update (e.g. '1'). This is completely internal and invisible to the user."`
	Status      string  `json:"status" jsonschema:"description=The new status of the task. Allowed values are: 'todo', 'in_progress', 'completed', 'failed'. This is completely internal and invisible to the user."`
	Description *string `json:"description,omitempty" jsonschema:"description=Optional new description for the task. This is completely internal and invisible to the user."`
}

type UpdateTaskOutput struct {
	Message string
}

type updateTaskTool struct {
	toolkit *CodingToolkit
}

func (t *updateTaskTool) Name() string { return "update_task" }
func (t *updateTaskTool) Description() string {
	return "Update the status and/or description of an existing task on your internal task list. This tool is strictly internal, and the user cannot see it or its results."
}
func (t *updateTaskTool) Schema() *UpdateTaskInput { return &UpdateTaskInput{} }
func (t *updateTaskTool) Execute(ctx context.Context, input *UpdateTaskInput) (*UpdateTaskOutput, error) {
	if input.ID == "" {
		return nil, fmt.Errorf("task ID cannot be empty")
	}
	if input.Status != "" && input.Status != "todo" && input.Status != "in_progress" && input.Status != "completed" && input.Status != "failed" {
		return nil, fmt.Errorf("invalid status: %q. Allowed statuses: 'todo', 'in_progress', 'completed', 'failed'", input.Status)
	}
	task, err := t.toolkit.tasks.Update(input.ID, input.Status, input.Description)
	if err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("Updated task ID %s to status %q.\n\n%s", task.ID, task.Status, formatTaskList(t.toolkit.tasks.List()))
	return &UpdateTaskOutput{Message: msg}, nil
}
func (t *updateTaskTool) ToString(o *UpdateTaskOutput) string {
	return o.Message
}

// list_tasks tool

type ListTasksInput struct {
	// Empty struct as list_tasks requires no arguments.
}

type ListTasksOutput struct {
	Message string
}

type listTasksTool struct {
	toolkit *CodingToolkit
}

func (t *listTasksTool) Name() string { return "list_tasks" }
func (t *listTasksTool) Description() string {
	return "List all tasks in your internal task list. This tool is strictly internal, and the user cannot see it or its results. Use it to keep track of your internal planning."
}
func (t *listTasksTool) Schema() *ListTasksInput { return &ListTasksInput{} }
func (t *listTasksTool) Execute(ctx context.Context, input *ListTasksInput) (*ListTasksOutput, error) {
	msg := formatTaskList(t.toolkit.tasks.List())
	return &ListTasksOutput{Message: msg}, nil
}
func (t *listTasksTool) ToString(o *ListTasksOutput) string {
	return o.Message
}
