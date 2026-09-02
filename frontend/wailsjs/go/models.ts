export namespace backup {
	
	export class BackupInfo {
	    Revision: number;
	    Size: number;
	    // Go type: time
	    At: any;
	
	    static createFrom(source: any = {}) {
	        return new BackupInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Revision = source["Revision"];
	        this.Size = source["Size"];
	        this.At = this.convertValues(source["At"], null);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class PullResult {
	    Action: string;
	    RemoteRev: number;
	    LocalRev: number;
	    BaselineRev: number;
	    RemoteDevice: string;
	    Forked: boolean;
	
	    static createFrom(source: any = {}) {
	        return new PullResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Action = source["Action"];
	        this.RemoteRev = source["RemoteRev"];
	        this.LocalRev = source["LocalRev"];
	        this.BaselineRev = source["BaselineRev"];
	        this.RemoteDevice = source["RemoteDevice"];
	        this.Forked = source["Forked"];
	    }
	}

}

export namespace config {
	
	export class Settings {
	    concurrency: number;
	    chunk_mib: number;
	    auto_backup: boolean;
	    outbox_push_fail: string;
	    size_padding: string;
	    cover_cache_mb: number;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.concurrency = source["concurrency"];
	        this.chunk_mib = source["chunk_mib"];
	        this.auto_backup = source["auto_backup"];
	        this.outbox_push_fail = source["outbox_push_fail"];
	        this.size_padding = source["size_padding"];
	        this.cover_cache_mb = source["cover_cache_mb"];
	    }
	}

}

export namespace index {
	
	export class Crumb {
	    ID: number;
	    Name: string;
	
	    static createFrom(source: any = {}) {
	        return new Crumb(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Name = source["Name"];
	    }
	}
	export class Entry {
	    ID: number;
	    IsFolder: boolean;
	    Name: string;
	    Size: number;
	    ModifiedAt: number;
	    State: string;
	    Pack: boolean;
	
	    static createFrom(source: any = {}) {
	        return new Entry(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.IsFolder = source["IsFolder"];
	        this.Name = source["Name"];
	        this.Size = source["Size"];
	        this.ModifiedAt = source["ModifiedAt"];
	        this.State = source["State"];
	        this.Pack = source["Pack"];
	    }
	}
	export class FileHit {
	    ID: number;
	    Name: string;
	    Path: string;
	    Note: string;
	    Tags: string[];
	    Size: number;
	    ModifiedAt: number;
	
	    static createFrom(source: any = {}) {
	        return new FileHit(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Name = source["Name"];
	        this.Path = source["Path"];
	        this.Note = source["Note"];
	        this.Tags = source["Tags"];
	        this.Size = source["Size"];
	        this.ModifiedAt = source["ModifiedAt"];
	    }
	}
	export class FileMeta {
	    Note: string;
	    Tags: string[];
	
	    static createFrom(source: any = {}) {
	        return new FileMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Note = source["Note"];
	        this.Tags = source["Tags"];
	    }
	}
	export class FileMetaUpdate {
	    Note?: string;
	    Tags: string[];
	
	    static createFrom(source: any = {}) {
	        return new FileMetaUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Note = source["Note"];
	        this.Tags = source["Tags"];
	    }
	}
	export class FolderHit {
	    ID: number;
	    Name: string;
	    Path: string;
	    Note: string;
	    Tags: string[];
	    CoverFileID: number;
	
	    static createFrom(source: any = {}) {
	        return new FolderHit(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Name = source["Name"];
	        this.Path = source["Path"];
	        this.Note = source["Note"];
	        this.Tags = source["Tags"];
	        this.CoverFileID = source["CoverFileID"];
	    }
	}
	export class FolderMeta {
	    Note: string;
	    UserMeta: string;
	    CoverFileID: number;
	    Tags: string[];
	
	    static createFrom(source: any = {}) {
	        return new FolderMeta(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Note = source["Note"];
	        this.UserMeta = source["UserMeta"];
	        this.CoverFileID = source["CoverFileID"];
	        this.Tags = source["Tags"];
	    }
	}
	export class FolderMetaUpdate {
	    Note?: string;
	    Cover?: number;
	    Tags: string[];
	
	    static createFrom(source: any = {}) {
	        return new FolderMetaUpdate(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Note = source["Note"];
	        this.Cover = source["Cover"];
	        this.Tags = source["Tags"];
	    }
	}
	export class FolderSummary {
	    PackCount: number;
	    FileCount: number;
	    TotalSize: number;
	    LatestAt: number;
	    PendingCount: number;
	    CoverFileIDs: number[];
	
	    static createFrom(source: any = {}) {
	        return new FolderSummary(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.PackCount = source["PackCount"];
	        this.FileCount = source["FileCount"];
	        this.TotalSize = source["TotalSize"];
	        this.LatestAt = source["LatestAt"];
	        this.PendingCount = source["PendingCount"];
	        this.CoverFileIDs = source["CoverFileIDs"];
	    }
	}

}

export namespace main {
	
	export class AppState {
	    Configured: boolean;
	    Unlocked: boolean;
	    FileCount: number;
	    HasLocalKeyfile: boolean;
	    DriveName: string;
	    DriveCount: number;
	
	    static createFrom(source: any = {}) {
	        return new AppState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Configured = source["Configured"];
	        this.Unlocked = source["Unlocked"];
	        this.FileCount = source["FileCount"];
	        this.HasLocalKeyfile = source["HasLocalKeyfile"];
	        this.DriveName = source["DriveName"];
	        this.DriveCount = source["DriveCount"];
	    }
	}
	export class DriveInfo {
	    ID: string;
	    Name: string;
	    URL: string;
	    Username: string;
	    RootPath: string;
	    RememberPassword: boolean;
	    Password: string;
	    Active: boolean;
	
	    static createFrom(source: any = {}) {
	        return new DriveInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Name = source["Name"];
	        this.URL = source["URL"];
	        this.Username = source["Username"];
	        this.RootPath = source["RootPath"];
	        this.RememberPassword = source["RememberPassword"];
	        this.Password = source["Password"];
	        this.Active = source["Active"];
	    }
	}
	export class DriveInput {
	    ID: string;
	    Name: string;
	    URL: string;
	    Username: string;
	    Password: string;
	    RootPath: string;
	    RememberPassword: boolean;
	
	    static createFrom(source: any = {}) {
	        return new DriveInput(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Name = source["Name"];
	        this.URL = source["URL"];
	        this.Username = source["Username"];
	        this.Password = source["Password"];
	        this.RootPath = source["RootPath"];
	        this.RememberPassword = source["RememberPassword"];
	    }
	}
	export class FileDetail {
	    ID: number;
	    UUID: string;
	    Name: string;
	    Path: string;
	    Size: number;
	    CipherSize: number;
	    SHA256: string;
	    ChunkSize: number;
	    BlobName: string;
	    State: string;
	    Pack: boolean;
	    CreatedAt: number;
	    ModifiedAt: number;
	    EncryptedAt?: number;
	    UploadedAt?: number;
	    Note: string;
	    Tags: string[];
	    HasThumb: boolean;
	
	    static createFrom(source: any = {}) {
	        return new FileDetail(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.UUID = source["UUID"];
	        this.Name = source["Name"];
	        this.Path = source["Path"];
	        this.Size = source["Size"];
	        this.CipherSize = source["CipherSize"];
	        this.SHA256 = source["SHA256"];
	        this.ChunkSize = source["ChunkSize"];
	        this.BlobName = source["BlobName"];
	        this.State = source["State"];
	        this.Pack = source["Pack"];
	        this.CreatedAt = source["CreatedAt"];
	        this.ModifiedAt = source["ModifiedAt"];
	        this.EncryptedAt = source["EncryptedAt"];
	        this.UploadedAt = source["UploadedAt"];
	        this.Note = source["Note"];
	        this.Tags = source["Tags"];
	        this.HasThumb = source["HasThumb"];
	    }
	}
	export class FolderView {
	    Crumbs: index.Crumb[];
	    Entries: index.Entry[];
	    Summaries: Record<number, index.FolderSummary>;
	
	    static createFrom(source: any = {}) {
	        return new FolderView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Crumbs = this.convertValues(source["Crumbs"], index.Crumb);
	        this.Entries = this.convertValues(source["Entries"], index.Entry);
	        this.Summaries = this.convertValues(source["Summaries"], index.FolderSummary, true);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class GCReport {
	    TrashOrDeleted: string[];
	    Orphans: string[];
	    CoverOrphans: string[];
	    DryRun: boolean;
	
	    static createFrom(source: any = {}) {
	        return new GCReport(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.TrashOrDeleted = source["TrashOrDeleted"];
	        this.Orphans = source["Orphans"];
	        this.CoverOrphans = source["CoverOrphans"];
	        this.DryRun = source["DryRun"];
	    }
	}
	export class SearchView {
	    Folders: index.FolderHit[];
	    Files: index.FileHit[];
	
	    static createFrom(source: any = {}) {
	        return new SearchView(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Folders = this.convertValues(source["Folders"], index.FolderHit);
	        this.Files = this.convertValues(source["Files"], index.FileHit);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class TestResult {
	    Ok: boolean;
	    Detail: string;
	
	    static createFrom(source: any = {}) {
	        return new TestResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Ok = source["Ok"];
	        this.Detail = source["Detail"];
	    }
	}
	export class ThumbData {
	    Data: number[];
	    Mime: string;
	
	    static createFrom(source: any = {}) {
	        return new ThumbData(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Data = source["Data"];
	        this.Mime = source["Mime"];
	    }
	}
	export class UnlockResult {
	    PulledRemoteKeyfile: boolean;
	    SuggestPullIndex: boolean;
	    FileCount: number;
	
	    static createFrom(source: any = {}) {
	        return new UnlockResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.PulledRemoteKeyfile = source["PulledRemoteKeyfile"];
	        this.SuggestPullIndex = source["SuggestPullIndex"];
	        this.FileCount = source["FileCount"];
	    }
	}
	export class WebDAVConfig {
	    URL: string;
	    Username: string;
	    Password: string;
	    RootPath: string;
	    RememberPassword: boolean;
	
	    static createFrom(source: any = {}) {
	        return new WebDAVConfig(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.URL = source["URL"];
	        this.Username = source["Username"];
	        this.Password = source["Password"];
	        this.RootPath = source["RootPath"];
	        this.RememberPassword = source["RememberPassword"];
	    }
	}

}

export namespace transfer {
	
	export class Transfer {
	    id: string;
	    kind: string;
	    name: string;
	    uuid: string;
	    phase: string;
	    bytesDone: number;
	    bytesTotal: number;
	    startedAt: number;
	    finishedAt: number;
	    err?: string;
	
	    static createFrom(source: any = {}) {
	        return new Transfer(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.kind = source["kind"];
	        this.name = source["name"];
	        this.uuid = source["uuid"];
	        this.phase = source["phase"];
	        this.bytesDone = source["bytesDone"];
	        this.bytesTotal = source["bytesTotal"];
	        this.startedAt = source["startedAt"];
	        this.finishedAt = source["finishedAt"];
	        this.err = source["err"];
	    }
	}

}

