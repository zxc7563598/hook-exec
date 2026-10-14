package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/zxc7563598/hook-exec/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"), false)
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// 全字段往返。这类测试专门用来抓"Updates 的 map 里列名写错了"这种问题 ——
// 写错列名 SQL 会直接报错或者静默不更新，光看代码很难发现。
func TestProjectUpdateRoundTrip(t *testing.T) {
	st := newTestStore(t)

	p := &model.Project{
		Name:       "初始名称",
		Repo:       "me/demo",
		Secret:     "sec1",
		Events:     "push",
		ShellFile:  "a.sh",
		WorkDir:    "/tmp/a",
		TimeoutSec: 30,
		Enabled:    true,
	}
	if err := st.CreateProject(p); err != nil {
		t.Fatalf("创建失败：%v", err)
	}
	if p.ID == 0 {
		t.Fatal("创建后应当回填 ID")
	}

	// 逐字段改成新值
	p.Name = "改名后"
	p.Repo = "me/other"
	p.Secret = "sec2"
	p.Events = "push,release"
	p.ShellFile = "b.sh"
	p.WorkDir = "/var/lib/b"
	p.TimeoutSec = 600
	p.Enabled = false

	if err := st.UpdateProject(p); err != nil {
		t.Fatalf("更新失败：%v", err)
	}

	got, err := st.GetProject(p.ID)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}

	checks := []struct {
		field     string
		got, want any
	}{
		{"Name", got.Name, "改名后"},
		{"Repo", got.Repo, "me/other"},
		{"Secret", got.Secret, "sec2"},
		{"Events", got.Events, "push,release"},
		{"ShellFile", got.ShellFile, "b.sh"},
		{"WorkDir", got.WorkDir, "/var/lib/b"},
		{"TimeoutSec", got.TimeoutSec, 600},
		{"Enabled", got.Enabled, false},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s 未被更新：期望 %v，得到 %v", c.field, c.want, c.got)
		}
	}
}

func TestSetProjectEnabled(t *testing.T) {
	st := newTestStore(t)
	p := &model.Project{Name: "n", Repo: "me/demo", Secret: "s", ShellFile: "a.sh", Enabled: true}
	if err := st.CreateProject(p); err != nil {
		t.Fatal(err)
	}

	if err := st.SetProjectEnabled(p.ID, false); err != nil {
		t.Fatalf("切换失败：%v", err)
	}
	got, _ := st.GetProject(p.ID)
	if got.Enabled {
		t.Error("应当已被停用")
	}

	// 不存在的 ID 要能区分出来，而不是静默成功
	if err := st.SetProjectEnabled(99999, true); err != ErrNotFound {
		t.Errorf("期望 ErrNotFound，得到 %v", err)
	}
}

func TestListEnabledProjectsByRepo(t *testing.T) {
	st := newTestStore(t)

	mk := func(name, repo string, enabled bool) {
		if err := st.CreateProject(&model.Project{
			Name: name, Repo: repo, Secret: "s", ShellFile: "a.sh", Enabled: enabled,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk("启用A", "me/demo", true)
	mk("停用B", "me/demo", false)
	mk("启用C", "me/demo", true)
	mk("别的仓库", "me/other", true)

	got, err := st.ListEnabledProjectsByRepo("me/demo")
	if err != nil {
		t.Fatal(err)
	}
	// 只应拿到启用中的两条，且按 ID 升序（保证处理顺序可复现）
	if len(got) != 2 {
		t.Fatalf("期望 2 条，得到 %d 条", len(got))
	}
	if got[0].Name != "启用A" || got[1].Name != "启用C" {
		t.Fatalf("结果或顺序不对：%v, %v", got[0].Name, got[1].Name)
	}

	n, err := st.CountDisabledProjectsByRepo("me/demo")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("停用项目数期望 1，得到 %d", n)
	}
}

func TestDeleteProjectKeepsExecutions(t *testing.T) {
	st := newTestStore(t)
	p := &model.Project{Name: "n", Repo: "me/demo", Secret: "s", ShellFile: "a.sh", Enabled: true}
	st.CreateProject(p)

	rec := &model.Execution{ProjectID: p.ID, ProjectName: p.Name, Repo: p.Repo,
		DeliveryID: "d1", Event: "push", Status: model.StatusSuccess}
	if _, err := st.CreateExecutionIfAbsent(rec); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteProject(p.ID); err != nil {
		t.Fatal(err)
	}

	// 记录是历史事实，项目删了也该留着
	if _, err := st.GetExecution(rec.ID); err != nil {
		t.Fatalf("执行记录不应当随项目一起删掉：%v", err)
	}
	if err := st.DeleteProject(p.ID); err != ErrNotFound {
		t.Errorf("重复删除期望 ErrNotFound，得到 %v", err)
	}
}

// GitHub 的 redeliver 会重复投递同一个 delivery，必须幂等。
func TestCreateExecutionIfAbsentIsIdempotent(t *testing.T) {
	st := newTestStore(t)

	base := func() *model.Execution {
		return &model.Execution{
			ProjectID: 1, ProjectName: "n", Repo: "me/demo",
			DeliveryID: "delivery-abc", Event: "push", Status: model.StatusPending,
		}
	}

	created, err := st.CreateExecutionIfAbsent(base())
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("第一次应当创建成功")
	}

	created, err = st.CreateExecutionIfAbsent(base())
	if err != nil {
		t.Fatalf("重复插入不应报错：%v", err)
	}
	if created {
		t.Fatal("同一个 (delivery, project) 第二次应当被判为重复")
	}

	// 换个项目，同样的 delivery 是合法的新记录
	other := base()
	other.ProjectID = 2
	created, err = st.CreateExecutionIfAbsent(other)
	if err != nil || !created {
		t.Fatalf("不同项目下的同一 delivery 应当可以创建：created=%v err=%v", created, err)
	}
}

func TestListExecutionsFilterAndPaginate(t *testing.T) {
	st := newTestStore(t)

	// 造 5 条记录：项目 1 三条（2 成功 1 失败），项目 2 两条（都成功）
	seed := []struct {
		projectID uint
		status    string
		delivery  string
	}{
		{1, model.StatusSuccess, "d1"},
		{1, model.StatusFailed, "d2"},
		{1, model.StatusSuccess, "d3"},
		{2, model.StatusSuccess, "d4"},
		{2, model.StatusSuccess, "d5"},
	}
	for _, s := range seed {
		if _, err := st.CreateExecutionIfAbsent(&model.Execution{
			ProjectID: s.projectID, ProjectName: "n", Repo: "me/demo",
			DeliveryID: s.delivery, Event: "push", Status: s.status,
			Output: "some output",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 全部
	_, total, err := st.ListExecutions(ExecutionFilter{})
	if err != nil || total != 5 {
		t.Fatalf("总数期望 5，得到 %d（err=%v）", total, err)
	}

	// 按项目
	_, total, _ = st.ListExecutions(ExecutionFilter{ProjectID: 1})
	if total != 3 {
		t.Fatalf("项目 1 期望 3 条，得到 %d", total)
	}

	// 按状态
	_, total, _ = st.ListExecutions(ExecutionFilter{Status: model.StatusFailed})
	if total != 1 {
		t.Fatalf("失败记录期望 1 条，得到 %d", total)
	}

	// 组合条件
	_, total, _ = st.ListExecutions(ExecutionFilter{ProjectID: 1, Status: model.StatusSuccess})
	if total != 2 {
		t.Fatalf("项目 1 的成功记录期望 2 条，得到 %d", total)
	}

	// 分页
	page1, total, _ := st.ListExecutions(ExecutionFilter{Page: 1, PageSize: 2})
	if total != 5 || len(page1) != 2 {
		t.Fatalf("第一页期望 2 条 / 总数 5，得到 %d / %d", len(page1), total)
	}
	page3, _, _ := st.ListExecutions(ExecutionFilter{Page: 3, PageSize: 2})
	if len(page3) != 1 {
		t.Fatalf("第三页期望 1 条，得到 %d", len(page3))
	}

	// 列表不带 output，省流量；正文只在详情接口里取
	if page1[0].Output != "" {
		t.Error("列表查询不应当带出 output 字段")
	}

	// page_size 必须封顶，否则一个超大值就能把内存拉满
	big, _, _ := st.ListExecutions(ExecutionFilter{PageSize: 1000000})
	if len(big) > MaxPageSize {
		t.Fatalf("page_size 未被封顶，返回了 %d 条", len(big))
	}
}

func TestDeleteExecutionsRequiresFilter(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.CreateExecutionIfAbsent(&model.Execution{
		ProjectID: 1, DeliveryID: "d1", Event: "push", Status: model.StatusSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	// 两个条件都不给时必须拒绝，免得一个手滑的请求清空整张表
	if _, err := st.DeleteExecutions(0, ""); err == nil {
		t.Fatal("无条件删除应当被拒绝")
	}

	// 给了条件才放行
	n, err := st.DeleteExecutions(1, "")
	if err != nil || n != 1 {
		t.Fatalf("期望删除 1 条，得到 %d（err=%v）", n, err)
	}
}

// 崩溃后会留下停在 pending/running 的记录，启动对账要把它们收尾，
// 否则页面上看起来永远在跑。
func TestReconcileOnStartup(t *testing.T) {
	st := newTestStore(t)

	for i, s := range []string{model.StatusPending, model.StatusRunning, model.StatusSuccess, model.StatusFailed} {
		if _, err := st.CreateExecutionIfAbsent(&model.Execution{
			ProjectID: 1, ProjectName: "n",
			DeliveryID: "d" + string(rune('0'+i)), Event: "push", Status: s,
		}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := st.ReconcileOnStartup()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("期望收尾 2 条，得到 %d", n)
	}

	records, _, _ := st.ListExecutions(ExecutionFilter{})
	for _, r := range records {
		if r.Status == model.StatusPending || r.Status == model.StatusRunning {
			t.Errorf("记录 %d 仍停在 %s", r.ID, r.Status)
		}
	}
	// 已经结束的记录不该被动到
	_, total, _ := st.ListExecutions(ExecutionFilter{Status: model.StatusSuccess})
	if total != 1 {
		t.Errorf("成功记录数被改动了：%d", total)
	}
}

func TestCleanupByAgeAndPerProject(t *testing.T) {
	st := newTestStore(t)

	// 项目 1 造 5 条，项目 2 造 3 条
	for i := 0; i < 5; i++ {
		st.CreateExecutionIfAbsent(&model.Execution{
			ProjectID: 1, DeliveryID: "p1-" + string(rune('a'+i)), Event: "push",
			Status: model.StatusSuccess,
		})
	}
	for i := 0; i < 3; i++ {
		st.CreateExecutionIfAbsent(&model.Execution{
			ProjectID: 2, DeliveryID: "p2-" + string(rune('a'+i)), Event: "push",
			Status: model.StatusSuccess,
		})
	}

	// 每条项目只留 2 条
	deleted, err := st.Cleanup(30, 2)
	if err != nil {
		t.Fatalf("清理失败：%v", err)
	}
	if deleted != 4 {
		t.Fatalf("期望删除 4 条（项目1 多 3 条、项目2 多 1 条），得到 %d", deleted)
	}

	for _, pid := range []uint{1, 2} {
		_, total, _ := st.ListExecutions(ExecutionFilter{ProjectID: pid})
		if total != 2 {
			t.Errorf("项目 %d 期望剩 2 条，得到 %d", pid, total)
		}
	}
	if _, err := st.Cleanup(30, 2); err != nil {
		t.Fatalf("重复清理不应报错：%v", err)
	}
}

func TestCleanupByAge(t *testing.T) {
	st := newTestStore(t)

	old := time.Now().AddDate(0, 0, -60)
	if err := st.db.Create(&model.Execution{
		ProjectID: 1, DeliveryID: "old", Event: "push",
		Status: model.StatusSuccess, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateExecutionIfAbsent(&model.Execution{
		ProjectID: 1, DeliveryID: "new", Event: "push", Status: model.StatusSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Cleanup(30, 500); err != nil {
		t.Fatal(err)
	}

	records, total, _ := st.ListExecutions(ExecutionFilter{})
	if total != 1 {
		t.Fatalf("60 天前的记录应当被清掉，剩 %d 条", total)
	}
	if records[0].DeliveryID != "new" {
		t.Fatalf("留下的应当是最近那条，实际是 %s", records[0].DeliveryID)
	}
}

func TestLatestExecutionsByProject(t *testing.T) {
	st := newTestStore(t)

	// 项目 1 有三条，项目 2 有一条
	for i, d := range []string{"a", "b", "c"} {
		if _, err := st.CreateExecutionIfAbsent(&model.Execution{
			ProjectID: 1, DeliveryID: d, Event: "push", Status: model.StatusSuccess,
			Output: "x",
		}); err != nil {
			t.Fatal(err)
		}
		_ = i
	}
	if _, err := st.CreateExecutionIfAbsent(&model.Execution{
		ProjectID: 2, DeliveryID: "z", Event: "push", Status: model.StatusFailed,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.LatestExecutionsByProject()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 个项目，得到 %d", len(got))
	}
	// 项目 1 最新的应当是 delivery "c"
	if got[1].DeliveryID != "c" {
		t.Errorf("项目 1 最新记录期望 c，得到 %s", got[1].DeliveryID)
	}
	if got[2].Status != model.StatusFailed {
		t.Errorf("项目 2 状态期望 failed，得到 %s", got[2].Status)
	}
	if got[1].Output != "" {
		t.Error("不该把 output 一起查出来，卡片上用不到")
	}
}
