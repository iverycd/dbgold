CREATE OR REPLACE procedure GETCURRENTNUMBER(NumberName_ nvarchar2,NumberFlag_ nvarchar2,FlowNumberLen number,CurrentNumber out nvarchar2)
as
maxValues number;
cnumber nvarchar2(50);
begin
   select  max(to_number( CurrentValue) ) into maxValues   from code_maxnumber where NumberName=NumberName_   and NumberFlag=NumberFlag_;
     if nvl(maxValues,0)=0 then
        begin
          dbms_output.put_line(' maxValues is null;');
           insert into code_maxnumber (NumberName,CurrentValue,NumberFlag) values (NumberName_,1,NumberFlag_);

        end;
        else
          begin
             update code_maxnumber set CurrentValue=to_char(to_number(CurrentValue)+1) where NumberName=NumberName_ and NumberFlag=NumberFlag_ ;
          end;
        end if;
            commit;
               dbms_output.put_line(maxvalues);
        if nvl(maxValues,0)=0 then
             begin
               select  NumberFlag_ || lpad('1',FlowNumberLen,'0') into  cnumber from dual;
               CurrentNumber:=cnumber;
               end;
        else
                 begin
                   maxValues:=maxValues+1;

                   dbms_output.put_line(maxvalues || 'maxvalues');
                   dbms_output.put_line(maxvalues || 'NumberFlag');

                 select  NumberFlag_ ||lpad(to_char(maxValues),FlowNumberLen, '0') into cnumber from dual;
                     CurrentNumber:=cnumber;
                   end;
                   end if;

  end;
