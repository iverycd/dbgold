CREATE OR REPLACE PROCEDURE EPOINT_PROC_ALTER()
AS
    v_schema_name VARCHAR(128);
    v_index_exists INT;
BEGIN
    -- 获取当前模式名
    v_schema_name := SF_GET_SCHEMA_NAME_BY_ID(CURRENT_SCHID());
    
    -- 检查索引是否存在
    SELECT COUNT(*) INTO v_index_exists 
    FROM ALL_INDEXES 
    WHERE TABLE_NAME = 'API_MANAGE_CONFIG' 
    AND INDEX_NAME = 'UK_API_MAN_CON_APIGUID' 
    AND OWNER = v_schema_name;
    
    -- 如果索引不存在则创建
    IF v_index_exists = 0 THEN
        EXECUTE IMMEDIATE 'ALTER TABLE API_MANAGE_CONFIG ADD CONSTRAINT UK_API_MAN_CON_APIGUID UNIQUE (APIGUID)';
        DBMS_OUTPUT.PUT_LINE('Unique index UK_API_MAN_CON_APIGUID created successfully.');
    ELSE
        DBMS_OUTPUT.PUT_LINE('Index UK_API_MAN_CON_APIGUID already exists.');
    END IF;
END;
