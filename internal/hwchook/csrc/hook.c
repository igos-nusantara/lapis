// liblapis_hwc.so: preloaded into Android's composer service.
//
// In per-app window mode the image's hwcomposer only gives a host window to
// layers named "TID:<task>#...". The framework adds that prefix to a
// SurfaceView only when the view was created with an Activity context, so
// games that create theirs with the Application context (SDL, some engines)
// end up with a bare "<pkg>/<activity>(BLAST)#n" layer that is dropped and
// their window stays black.
//
// This library interposes the hwcomposer's layer-name parser. It remembers
// which task each activity window belongs to and gives untagged layers of
// that activity (or, failing that, of the same package) the task prefix.
//
// Built freestanding (no libc headers, no NDK): see build.sh.

typedef unsigned long size_t;

// libc++ std::string (little endian, standard layout).
struct str {
	size_t w0, w1;
	char *w2;
};

// hwcomposer's layer_info: { LayerSplitType type; string aid, tid, component; }
struct layer_info {
	int type;
	struct str aid, tid, component;
};

typedef struct layer_info (*split_fn)(const struct str *);

typedef struct {
	const char *dli_fname;
	void *dli_fbase;
	const char *dli_sname;
	void *dli_saddr;
} Dl_info;

int dladdr(const void *addr, Dl_info *info);
void *dlopen(const char *filename, int flags);
void *dlsym(void *handle, const char *symbol);

#define RTLD_NOW 2
#define RTLD_NOLOAD 4

#define SPLIT "_ZN24split_layer_names_helper16split_layer_nameERKNSt3__112basic_stringIcNS0_11char_traitsIcEENS0_9allocatorIcEEEE"

#define TITLE_MAX 240
#define TID_MAX 12
#define SLOTS 64
// Every composition parses the names of all layers, so a live task's window
// layer is seen at least once per frame. A task not seen for this many
// parses is gone: its untagged layers must not open a window of their own.
#define STALE 512

struct task {
	char title[TITLE_MAX]; // "<pkg>/<activity>"
	char tid[TID_MAX];
	size_t seen; // value of calls when last seen; 0 for a free slot
};

static struct task tasks[SLOTS];
static size_t calls;
static split_fn orig;

static const char *data(const struct str *s, size_t *n) {
	const unsigned char *b = (const unsigned char *)s;
	if (b[0] & 1) {
		*n = s->w1;
		return s->w2;
	}
	*n = b[0] >> 1;
	return (const char *)b + 1;
}

static int has_prefix(const char *s, size_t n, const char *p) {
	size_t i;
	for (i = 0; p[i]; i++)
		if (i >= n || s[i] != p[i])
			return 0;
	return 1;
}

static int eq(const char *a, size_t n, const char *b) {
	size_t i;
	for (i = 0; i < n; i++)
		if (b[i] != a[i] || !b[i])
			return 0;
	return b[n] == 0;
}

static int same(const char *a, const char *b, size_t n) {
	size_t i;
	for (i = 0; i < n; i++)
		if (a[i] != b[i])
			return 0;
	return 1;
}

static void copy(char *dst, const char *src, size_t n) {
	size_t i;
	for (i = 0; i < n; i++)
		dst[i] = src[i];
	dst[n] = 0;
}

// title_len returns the length of the "<pkg>/<activity>" part of a layer
// name: everything before "(", "$" or the trailing "#<n>".
static size_t title_len(const char *s, size_t n) {
	size_t i, end = n;
	for (i = 0; i < n; i++) {
		if (s[i] == '(' || s[i] == '$')
			return i;
		if (s[i] == '#')
			end = i;
	}
	return end;
}

static size_t pkg_len(const char *s, size_t n) {
	size_t i;
	for (i = 0; i < n; i++)
		if (s[i] == '/')
			return i;
	return 0;
}

static void remember(const char *name, size_t n) {
	size_t i, hash, tl;
	struct task *t = &tasks[0];
	for (hash = 4; hash < n && name[hash] != '#'; hash++)
		;
	if (hash == n || hash == 4 || hash - 4 >= TID_MAX)
		return;
	tl = title_len(name + hash + 1, n - hash - 1);
	if (tl == 0 || tl >= TITLE_MAX || !pkg_len(name + hash + 1, tl))
		return;
	for (i = 0; i < SLOTS; i++) {
		if (eq(name + hash + 1, tl, tasks[i].title)) {
			t = &tasks[i];
			break;
		}
		if (tasks[i].seen < t->seen)
			t = &tasks[i];
	}
	copy(t->title, name + hash + 1, tl);
	copy(t->tid, name + 4, hash - 4);
	t->seen = calls;
}

// lookup finds the task of an untagged layer: same activity first, then the
// most recently seen task of the same package.
static const char *lookup(const char *name, size_t n) {
	size_t i, tl = title_len(name, n), pl = pkg_len(name, tl);
	struct task *best = 0;
	if (!pl)
		return 0;
	for (i = 0; i < SLOTS; i++) {
		if (!tasks[i].seen || calls - tasks[i].seen > STALE)
			continue;
		if (eq(name, tl, tasks[i].title))
			return tasks[i].tid;
		if (pkg_len(tasks[i].title, TITLE_MAX) == pl && same(name, tasks[i].title, pl) &&
		    (!best || tasks[i].seen > best->seen))
			best = &tasks[i];
	}
	return best ? best->tid : 0;
}

struct layer_info split_layer_names_helper_split(const struct str *name) __asm__(SPLIT);

struct layer_info split_layer_names_helper_split(const struct str *name) {
	char buf[512];
	struct str fake;
	size_t n, i, k;
	const char *s, *tid;

	if (!orig) {
		Dl_info info;
		void *h;
		if (dladdr(__builtin_return_address(0), &info) && info.dli_fname &&
		    (h = dlopen(info.dli_fname, RTLD_NOW | RTLD_NOLOAD)))
			orig = (split_fn)dlsym(h, SPLIT);
		if (!orig || orig == split_layer_names_helper_split)
			__builtin_trap();
	}

	calls++;
	s = data(name, &n);
	if (has_prefix(s, n, "TID:")) {
		remember(s, n);
		return orig(name);
	}
	if (!(tid = lookup(s, n)))
		return orig(name);

	// "TID:<tid>#<name>"
	k = 0;
	for (i = 0; "TID:"[i]; i++)
		buf[k++] = "TID:"[i];
	for (i = 0; tid[i]; i++)
		buf[k++] = tid[i];
	buf[k++] = '#';
	if (k + n >= sizeof(buf))
		return orig(name);
	for (i = 0; i < n; i++)
		buf[k++] = s[i];
	buf[k] = 0;
	fake.w0 = (k + 1) | 1; // long mode; only read through a const reference
	fake.w1 = k;
	fake.w2 = buf;
	return orig(&fake);
}
