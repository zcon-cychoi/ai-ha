> 원본 설계 노트다. 현재 구현과 다른 점(2026-10-13): 저장소 메타데이터의 **복구 기준은 세트별
> `manifest.json`** 이고 시스템별 SQLite는 조회용 색인이다([data-model.md](data-model.md) 1절, 8절).
> `http` 저장소는 미구현, gzip 코덱은 이름만 정의되어 있다. 파일 이름의 random id는 세트 ID와 별개다.

### 리눅스 백업

* tar, zstd(default) 아카이빙 방식으로 백업.
* 압축 포맷은 비압축(no-compressed), gzip 등으로 옵션을 지정할 수 있는 확장 대비할 것
  (당장은 zstd 하나만 쓸 것이지만 혹시 모를 확장에 대비한 준비)
* 파티션 기준으로 백업을 받을 것
  * /dev/sda1 (bind, read only 마운트 이후 해당 경로에서 백업)
    * /mnt/sda1-{random ID} 임시 디렉토리 생성
    * mount -B [/dev/sda1의 마운트 포인트] [임시 디렉토리]
    * mount -o remount,ro,bind [임시 디렉토리]
    * 마운트된 디렉토리를 백업
* 파일명
  * 확장자는 aha
  * 파일 이름은 hostname, 파티션(sda1, rl-root, lv-data 등), ramdom id
* 백업 수행 중 스트림은 로컬, sftp, http, smb/cifs, s3 프로토콜 실시간 전송을 지원



### 저장소

* 백업 저장소는 local, sftp, http, smb/cifs,  s3 프로토콜을 지원해야함.
* 대상 시스템 당 하나의 디렉토리를 가지고 그곳에 백업 파일을 보관
* 시스템에 대한 메타데이터 및 백업 파일에 대한 정보들을 sqlite 를 이용해서 저장
  백업 대상 시스템 당 하나씩 sqlite 파일을 가지고 있는 형태
  * 정보 조회는 이 데이터 베이스 파일을 바탕으로...



### 컨테이너 백업

* `velero` 사용