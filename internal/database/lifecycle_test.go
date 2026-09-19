package database

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/skymail-backend/internal/testpostgres"
)

func lifecycleStore(t *testing.T) *Store {
	t.Helper()
	pool := testpostgres.Start(t)
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate lifecycle test")
	}
	migrationsDir := filepath.Join(filepath.Dir(filename), "..", "..", "db", "migrations")
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, file := range files {
		migration, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), string(migration)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(file), err)
		}
	}
	return NewStore(pool)
}

func TestTemplateAndMailingListLifecyclePreservesMailHistory(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	firstActor := "f75c1760-d038-4782-834f-1ec816557a86"
	secondActor := "d49619ce-0a81-487f-a1dc-6884fe79ef18"

	template, err := db.CreateTemplate(ctx, CreateTemplateParams{
		Name: "Etkinlik duyurusu", Subject: "Merhaba {{.FullName}}",
		HtmlContent: "<p>Merhaba {{.FullName}}</p>", PlainTextContent: "Merhaba {{.FullName}}",
		ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateMailingList(ctx, "AGC katılımcıları")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := db.AddRecipientToMailingList(ctx, AddRecipientToMailingListParams{
		MailListID: list.ID, FullName: "Ada Lovelace", Email: "ada@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	taskRows, err := db.CreateMailTask(ctx, CreateMailTaskParams{
		SentBy: "operator", TemplateID: &template.ID, MailListID: &list.ID, BodyVariables: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(taskRows) != 1 {
		t.Fatalf("created mail task rows = %d, want 1", len(taskRows))
	}
	taskID := taskRows[0].TaskID
	if _, err := db.CreateMailQueueItems(ctx, []CreateMailQueueItemsParams{{
		TaskID: taskID, RecipientFullName: "Ada Lovelace", RecipientEmail: "ada@example.com",
		Subject: "Merhaba Ada", Body: "Merhaba Ada",
	}}); err != nil {
		t.Fatal(err)
	}

	archivedTemplate, err := db.ArchiveTemplate(ctx, ArchiveTemplateParams{ID: template.ID, ArchivedBy: &firstActor})
	if err != nil {
		t.Fatal(err)
	}
	archivedList, err := db.ArchiveMailingList(ctx, ArchiveMailingListParams{ID: list.ID, ArchivedBy: &firstActor})
	if err != nil {
		t.Fatal(err)
	}
	if archivedTemplate.ArchivedAt == nil || archivedTemplate.ArchivedBy == nil || *archivedTemplate.ArchivedBy != firstActor {
		t.Fatalf("archived template metadata = %+v", archivedTemplate)
	}
	if archivedList.ArchivedAt == nil || archivedList.ArchivedBy == nil || *archivedList.ArchivedBy != firstActor {
		t.Fatalf("archived mailing list metadata = %+v", archivedList)
	}

	archivedTemplateAgain, err := db.ArchiveTemplate(ctx, ArchiveTemplateParams{ID: template.ID, ArchivedBy: &secondActor})
	if err != nil {
		t.Fatal(err)
	}
	archivedListAgain, err := db.ArchiveMailingList(ctx, ArchiveMailingListParams{ID: list.ID, ArchivedBy: &secondActor})
	if err != nil {
		t.Fatal(err)
	}
	if !archivedTemplateAgain.ArchivedAt.Equal(*archivedTemplate.ArchivedAt) || *archivedTemplateAgain.ArchivedBy != firstActor {
		t.Fatalf("second template archive changed audit metadata: before=%+v after=%+v", archivedTemplate, archivedTemplateAgain)
	}
	if !archivedListAgain.ArchivedAt.Equal(*archivedList.ArchivedAt) || *archivedListAgain.ArchivedBy != firstActor {
		t.Fatalf("second list archive changed audit metadata: before=%+v after=%+v", archivedList, archivedListAgain)
	}

	if _, err := db.GetTemplateById(ctx, template.ID); err != pgx.ErrNoRows {
		t.Fatalf("default template read error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := db.GetMailingListById(ctx, list.ID); err != pgx.ErrNoRows {
		t.Fatalf("default list read error = %v, want pgx.ErrNoRows", err)
	}
	if got, err := db.GetAllTemplates(ctx, GetAllTemplatesParams{Limit: 10}); err != nil || len(got) != 0 {
		t.Fatalf("default templates = %+v, err = %v", got, err)
	}
	if got, err := db.GetAllMailingLists(ctx, GetAllMailingListsParams{Limit: 10}); err != nil || len(got) != 0 {
		t.Fatalf("default mailing lists = %+v, err = %v", got, err)
	}
	if got, err := db.GetArchivedTemplates(ctx, GetArchivedTemplatesParams{Limit: 10}); err != nil || len(got) != 1 {
		t.Fatalf("archived templates = %+v, err = %v", got, err)
	}
	if got, err := db.GetArchivedMailingLists(ctx, GetArchivedMailingListsParams{Limit: 10}); err != nil || len(got) != 1 {
		t.Fatalf("archived mailing lists = %+v, err = %v", got, err)
	}
	if count, err := db.CountRecipientsByMailingListId(ctx, list.ID); err != nil || count != 0 {
		t.Fatalf("default archived list recipient count = %d, err = %v", count, err)
	}

	task, err := db.GetMailTaskById(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.TemplateName == nil || *task.TemplateName != template.Name || task.MailListName == nil || *task.MailListName != list.Name {
		t.Fatalf("archived source names missing from mail task history: %+v", task)
	}
	queue, err := db.GetMailQueueItemsByTaskId(ctx, GetMailQueueItemsByTaskIdParams{TaskID: taskID, Limit: 10})
	if err != nil || len(queue) != 1 {
		t.Fatalf("mail queue history = %+v, err = %v", queue, err)
	}

	if _, err := db.RestoreTemplate(ctx, template.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RestoreTemplate(ctx, template.ID); err != nil {
		t.Fatalf("second template restore: %v", err)
	}
	if _, err := db.RestoreMailingList(ctx, list.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RestoreMailingList(ctx, list.ID); err != nil {
		t.Fatalf("second list restore: %v", err)
	}
	if count, err := db.CountRecipientsByMailingListId(ctx, list.ID); err != nil || count != 1 {
		t.Fatalf("restored list recipient count = %d, err = %v", count, err)
	}

	if err := db.RemoveRecipientFromMailingListByID(ctx, RemoveRecipientFromMailingListByIDParams{MailListID: list.ID, RecipientID: recipient.ID}); err != nil {
		t.Fatal(err)
	}
	if count, err := db.CountRecipientsByMailingListId(ctx, list.ID); err != nil || count != 0 {
		t.Fatalf("recipient membership was not physically removed: count=%d err=%v", count, err)
	}
}

func TestInactiveTemplateAndMailingListCannotCreateNewMailTask(t *testing.T) {
	db := lifecycleStore(t)
	ctx := context.Background()
	actor := "82ab9193-6e51-4e9e-9b93-32120c44a19e"

	template, err := db.CreateTemplate(ctx, CreateTemplateParams{
		Name: "Arşiv", Subject: "Konu", HtmlContent: "<p>İçerik</p>",
		PlainTextContent: "İçerik", ReactEmailContent: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	list, err := db.CreateMailingList(ctx, "Arşiv liste")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddRecipientToMailingList(ctx, AddRecipientToMailingListParams{
		MailListID: list.ID, FullName: "Grace Hopper", Email: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArchiveTemplate(ctx, ArchiveTemplateParams{ID: template.ID, ArchivedBy: &actor}); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.CreateMailTask(ctx, CreateMailTaskParams{
		SentBy: "operator", TemplateID: &template.ID, MailListID: &list.ID, BodyVariables: []byte(`{}`),
	}); err != nil || len(rows) != 0 {
		t.Fatalf("mail task created with archived template: rows=%+v err=%v", rows, err)
	}

	if _, err := db.RestoreTemplate(ctx, template.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ArchiveMailingList(ctx, ArchiveMailingListParams{ID: list.ID, ArchivedBy: &actor}); err != nil {
		t.Fatal(err)
	}
	if rows, err := db.CreateMailTask(ctx, CreateMailTaskParams{
		SentBy: "operator", TemplateID: &template.ID, MailListID: &list.ID, BodyVariables: []byte(`{}`),
	}); err != nil || len(rows) != 0 {
		t.Fatalf("mail task created with archived list: rows=%+v err=%v", rows, err)
	}
	if count, err := db.CountMailTasks(ctx); err != nil || count != 0 {
		t.Fatalf("inactive source left empty mail tasks: count=%d err=%v", count, err)
	}
}
